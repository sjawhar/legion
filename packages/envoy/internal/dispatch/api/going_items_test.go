package api

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// seededItemTexts is what seedGoingItemText wrote: one hit of every kind search ranks, with the
// text each holds, plus the ids an answer must find nothing for.
type seededItemTexts struct {
	hits       []going.Hit
	texts      map[string]string
	issueTitle string
	issueSpec  string
}

// seedGoingItemText writes one item of every kind search ranks, each with text of its own, a
// comment holding only white space, and gives back what goingItemText must answer for them. The
// texts carry characters outside ASCII so a cut by bytes could not pass for a cut by runes.
func seedGoingItemText(t *testing.T, database *store.Store) seededItemTexts {
	t.Helper()
	ctx := context.Background()
	const project, issueKey = "ITEM", "ITEM-1"
	seeded := seededItemTexts{
		texts:      map[string]string{},
		issueTitle: "Ünïcödé title — 日本語",
		issueSpec:  "The spec's own words: naïve café.",
	}
	mustExecItemText(t, database.Pool, `insert into projects (key, name) values ($1, 'Items')`, project)
	mustExecItemText(t, database.Pool, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, $3, '{"kind":"user","id":"alice"}', 'A')
	`, issueKey, project, seeded.issueTitle)
	seedItemTextSpec(t, database.Pool, project, issueKey, seeded.issueSpec)
	// issue_embedding_text composes the issue's text out of its title and the markdown of the
	// latest version of its primary document, which is what meaning search embedded and so what
	// an answer is shown.
	seeded.texts["issue:"+issueKey] = seeded.issueTitle + "\n\n" + seeded.issueSpec

	var document string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by, ref_key)
		values ($1, 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', $1 || '/notes-md')
		returning id::text
	`, project).Scan(&document); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	addItemTextVersion(t, database.Pool, document, 1, "# An earlier draft")
	addItemTextVersion(t, database.Pool, document, 2, "# The latest — ✓")
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
	// The ask's text is the text its search vector is built from: the question, then
	// ask_search_suffix's option labels and descriptions, the answer's text and its selected
	// labels, each part separated by a space and an absent part empty (migration 0019).
	seeded.texts["ask:"+ask] = "Which region? eu eu, for latency "

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

	seeded.hits = []going.Hit{
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

func mustExecItemText(t *testing.T, pool *store.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(sql), err)
	}
}

// seedItemTextSpec gives issueKey its primary document, as issue creation does, with spec as its
// first version.
func seedItemTextSpec(t *testing.T, pool *store.Pool, project, issueKey, spec string) {
	t.Helper()
	var artifactID string
	if err := pool.QueryRow(context.Background(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issueKey, project).Scan(&artifactID); err != nil {
		t.Fatalf("seed the primary document of %s: %v", issueKey, err)
	}
	addItemTextVersion(t, pool, artifactID, 1, spec)
}

func addItemTextVersion(t *testing.T, pool *store.Pool, artifactID string, number int, markdown string) {
	t.Helper()
	mustExecItemText(t, pool, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, $2, $3, '[]')`,
		artifactID, number, markdown)
}

// Every kind search ranks answers that item's own stored text: an issue its title and its spec as
// issue_embedding_text composes them, which is what meaning search embedded for it, a document
// the markdown of its latest version, a comment and a message their bodies, and an ask the
// question and suffix search ranks it by. An item the database holds nothing for is absent from
// the answer, and so is one whose text is only white space, which matches nothing by meaning
// either.
func TestGoingItemTextAnswersEachKindsOwnText(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedGoingItemText(t, database)
	texts, err := goingItemText(context.Background(), database.Pool, seeded.hits, contracts.GoingHitRuneMax)
	if err != nil {
		t.Fatalf("goingItemText: %v", err)
	}
	for key, want := range seeded.texts {
		if got := texts[key]; got != want {
			t.Errorf("%s reads %q, want %q", key, got, want)
		}
	}
	if len(texts) != len(seeded.texts) {
		t.Fatalf("goingItemText answered %d items (%v), want the %d the database holds text for: an item it finds nothing for and a white-space one are absent",
			len(texts), texts, len(seeded.texts))
	}
}

// The cut is by runes and not by bytes, so an answer shown a text of two-byte and four-byte
// characters is shown the characters asked for.
func TestGoingItemTextCutsToRunes(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedGoingItemText(t, database)
	const runes = 7
	texts, err := goingItemText(context.Background(), database.Pool, seeded.hits, runes)
	if err != nil {
		t.Fatalf("goingItemText: %v", err)
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

// The cut is the statement's own, and it lands on the character boundary the Go helper's cut
// landed on: left counts characters in a UTF-8 database, so a budget falling between one-, two-,
// three- and four-byte characters answers the bytes text.HeadRunes answered of the whole value.
func TestGoingItemTextCutsWhereTheRuneHelperCut(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedGoingItemText(t, database)
	// A text whose characters are of all four widths, so a cut inside it says which unit the
	// statement counted in: bytes, UTF-16 units or characters.
	const satellites = "Déjà vu — 🛰🛰 two satellites"
	var wide string
	if err := database.Pool.QueryRow(context.Background(), `
		insert into comments (issue_key, author, body) values ('ITEM-1', '{"kind":"user","id":"alice"}', $1) returning id::text
	`, satellites).Scan(&wide); err != nil {
		t.Fatalf("seed the four-byte comment: %v", err)
	}
	hits := append(seeded.hits, going.Hit{Kind: "comment", ID: wide})
	seeded.texts["comment:"+wide] = satellites
	for _, runes := range []int{1, 2, 9, 10, 11, 12, 13} {
		texts, err := goingItemText(context.Background(), database.Pool, hits, runes)
		if err != nil {
			t.Fatalf("goingItemText cut to %d runes: %v", runes, err)
		}
		for key, whole := range seeded.texts {
			want := text.HeadRunes(whole, runes)
			if got := texts[key]; got != want {
				t.Errorf("%s cut to %d runes reads %q (%d bytes), want the helper's %q (%d bytes)",
					key, runes, got, len(got), want, len(want))
			}
		}
	}
}

// A page of hits costs one statement whatever kinds it names, and a page naming none makes no
// statement at all.
func TestGoingItemTextReadsOnePageInOneStatement(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedGoingItemText(t, database)
	recorder := &storetest.StatementRecorder{}
	pool := storetest.Traced(t, database.Pool, recorder, nil)
	if _, err := goingItemText(context.Background(), pool, seeded.hits, 100); err != nil {
		t.Fatalf("goingItemText: %v", err)
	}
	if statements := recorder.Statements(); len(statements) != 1 {
		t.Fatalf("one page of %d hits made %d statements (%v), want one", len(seeded.hits), len(statements), statements)
	}
	texts, err := goingItemText(context.Background(), pool, nil, 100)
	if err != nil || len(texts) != 0 {
		t.Fatalf("goingItemText over no hits = %v, %v; want an empty answer and no error", texts, err)
	}
	if statements := recorder.Statements(); len(statements) != 1 {
		t.Fatalf("a page of no hits made %d statements (%v), want none of its own", len(statements)-1, statements[1:])
	}
}

// The kinds this read answers are search's own, and it answers them from the arms search's txt
// lateral reads a hit's text with: the two must stay in sync, so that the text a model is given
// for a hit is the text search ranked and showed the reader a snippet of.
func TestGoingItemTextCoversSearchsOwnKinds(t *testing.T) {
	page := searchPageAndTailSQL("legs")
	for _, kind := range []string{"issue", "document", "comment", "ask", "message"} {
		arm := "when '" + kind + "' then"
		if !strings.Contains(goingItemTextQuery, arm) {
			t.Errorf("the going item text read has no arm for %s", kind)
		}
		if !strings.Contains(page, arm) {
			t.Errorf("search's page has no arm for %s: the two reads must stay in sync", kind)
		}
	}
}
