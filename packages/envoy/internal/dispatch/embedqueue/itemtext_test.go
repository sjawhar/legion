package embedqueue

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// seededItems is what seedItemText wrote: one item of every kind meaning search covers, with the
// text each holds, plus the ids an answer must find nothing for.
type seededItems struct {
	items      []Item
	texts      map[string]string
	issueTitle string
	issueSpec  string
}

// seedItemText writes one item of every kind coverageSources lists, each with text of its own, a
// comment holding only white space, and gives back what ItemText must answer for them. The texts
// carry characters outside ASCII so a cut by bytes could not pass for a cut by runes.
func seedItemText(t *testing.T, database *store.Store) seededItems {
	t.Helper()
	ctx := context.Background()
	const project, issueKey = "ITEM", "ITEM-1"
	seeded := seededItems{
		texts:      map[string]string{},
		issueTitle: "Ünïcödé title — 日本語",
		issueSpec:  "The spec's own words: naïve café.",
	}
	mustExec(t, database.Pool, `insert into projects (key, name) values ($1, 'Items')`, project)
	mustExec(t, database.Pool, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, $3, '{"kind":"user","id":"alice"}', 'A')
	`, issueKey, project, seeded.issueTitle)
	seedSpec(t, database.Pool, project, issueKey, seeded.issueSpec)
	// issue_embedding_text composes the issue's text out of its title and the markdown of the
	// latest version of its primary document, which is what search ranked and so what an answer
	// is shown.
	seeded.texts["issue:"+issueKey] = seeded.issueTitle + "\n\n" + seeded.issueSpec

	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by, ref_key)
		values ($1, 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', $1 || '/notes-md')
		returning id::text
	`, project).Scan(&document); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	addVersion(t, database.Pool, document, 1, "# An earlier draft")
	addVersion(t, database.Pool, document, 2, "# The latest — ✓")
	seeded.texts["document:"+document] = "# The latest — ✓"

	var comment string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, issueKey, "A comment — ü").Scan(&comment); err != nil {
		t.Fatalf("seed the comment: %v", err)
	}
	seeded.texts["comment:"+comment] = "A comment — ü"

	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, options, state, answer)
		values ($1, '{"kind":"session","id":"asker"}', 'Which region?', '[{"label":"eu"}]', 'answered', '{"text":"eu, for latency","option":"eu"}')
		returning id::text
	`, issueKey).Scan(&ask); err != nil {
		t.Fatalf("seed the ask: %v", err)
	}
	seeded.texts["ask:"+ask] = "Which region? eu, for latency"

	var message string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2) returning id::text
	`, issueKey, "A message — ✉").Scan(&message); err != nil {
		t.Fatalf("seed the message: %v", err)
	}
	seeded.texts["message:"+message] = "A message — ✉"

	var blank string
	if err := database.Pool.QueryRow(ctx, `
		insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', '   ') returning id::text
	`, issueKey).Scan(&blank); err != nil {
		t.Fatalf("seed the white-space comment: %v", err)
	}

	seeded.items = []Item{
		{Kind: "issue", ID: issueKey},
		{Kind: "document", ID: document},
		{Kind: "comment", ID: comment},
		{Kind: "ask", ID: ask},
		{Kind: "message", ID: message},
		{Kind: "comment", ID: blank},
		{Kind: "comment", ID: "00000000-0000-4000-8000-00000000000a"},
		{Kind: "issue", ID: "ITEM-404"},
	}
	return seeded
}

// Every kind search ranks answers that item's own stored text, the text its vector was made
// from: an issue its title and its spec as issue_embedding_text composes them, a document the
// markdown of its latest version, a comment and a message their bodies, and an ask its question
// and its answer. An item the database holds nothing for is absent from the answer, and so is
// one whose text is only white space, which matches nothing by meaning either.
func TestItemTextAnswersEachKindsOwnText(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedItemText(t, database)
	texts, err := ItemText(context.Background(), database.Pool, seeded.items, contracts.GoingHitRuneMax)
	if err != nil {
		t.Fatalf("ItemText: %v", err)
	}
	for key, want := range seeded.texts {
		if got := texts[key]; got != want {
			t.Errorf("%s reads %q, want %q", key, got, want)
		}
	}
	if len(texts) != len(seeded.texts) {
		t.Fatalf("ItemText answered %d items (%v), want the %d the database holds text for: an item it finds nothing for and a white-space one are absent",
			len(texts), texts, len(seeded.texts))
	}
}

// The cut is by runes and not by bytes, so an answer shown a text of two-byte and four-byte
// characters is shown the characters asked for.
func TestItemTextCutsToRunes(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedItemText(t, database)
	const runes = 7
	texts, err := ItemText(context.Background(), database.Pool, seeded.items, runes)
	if err != nil {
		t.Fatalf("ItemText: %v", err)
	}
	for key, whole := range seeded.texts {
		want := string([]rune(whole)[:runes])
		if got := texts[key]; got != want {
			t.Errorf("%s cut to %d runes reads %q, want %q", key, runes, got, want)
		}
	}
	if got := texts["issue:ITEM-1"]; len([]rune(got)) != runes || len(got) == runes {
		t.Fatalf("the issue's cut text is %q: %d runes in %d bytes, want %d runes of a text the bytes would cut differently",
			got, len([]rune(got)), len(got), runes)
	}
}

// A page of items costs one statement whatever kinds it names, and a page naming none makes no
// statement at all.
func TestItemTextReadsOnePageInOneStatement(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedItemText(t, database)
	recorder := &storetest.StatementRecorder{}
	pool := storetest.Traced(t, database.Pool, recorder, nil)
	if _, err := ItemText(context.Background(), pool, seeded.items, 100); err != nil {
		t.Fatalf("ItemText: %v", err)
	}
	if statements := recorder.Statements(); len(statements) != 1 {
		t.Fatalf("one page of %d items made %d statements (%v), want one", len(seeded.items), len(statements), statements)
	}
	texts, err := ItemText(context.Background(), pool, nil, 100)
	if err != nil || len(texts) != 0 {
		t.Fatalf("ItemText over no items = %v, %v; want an empty answer and no error", texts, err)
	}
	if statements := recorder.Statements(); len(statements) != 1 {
		t.Fatalf("a page of no items made %d statements (%v), want none of its own", len(statements)-1, statements[1:])
	}
}

// The kinds the read covers are coverageSources' own, so the text an answer shows is the text
// search ranked and no second list can drift from it.
func TestItemTextCoversEveryCoveredKind(t *testing.T) {
	for _, source := range coverageSources {
		if !strings.Contains(itemTextQuery, "w.kind = '"+source.kind+"'") {
			t.Errorf("the item text read has no arm for %s", source.kind)
		}
	}
}
