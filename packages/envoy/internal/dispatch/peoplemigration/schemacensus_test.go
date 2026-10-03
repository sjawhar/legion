package peoplemigration

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A login the map names, left by the move in a column no field of the move knows - a table added
// after this code was written - fails the run, which then changes nothing: in a column of every
// type the census searches, whether the column holds it as its whole value, as an array's element,
// or anywhere in a JSON value, an object's key in any case at any depth included.
func TestRunRefusesALoginLeftInAColumnItDoesNotKnow(t *testing.T) {
	for _, test := range []struct{ name, create, insert, hit string }{
		{"a text column", `create table later_owners (id int primary key, owner text)`,
			`insert into later_owners values (1, 'Ada-Example'), (2, 'carol@example.com')`, "later_owners.owner (1 rows)"},
		{"a character varying column", `create table later_assignees (id int primary key, assignee varchar(64))`,
			`insert into later_assignees values (1, 'Ada-Example'), (2, 'carol@example.com')`, "later_assignees.assignee (1 rows)"},
		{"a character column", `create table later_leads (id int primary key, owner character(32))`,
			`insert into later_leads values (1, 'carol@example.com'), (2, 'Ada-Example')`, "later_leads.owner (1 rows)"},
		{"a citext column", `create extension if not exists citext; create table later_handles (id int primary key, handle citext)`,
			`insert into later_handles values (1, 'BOB-EXAMPLE')`, "later_handles.handle (1 rows)"},
		{"a character varying array", `create table later_watchers (id int primary key, watchers varchar(64)[])`,
			`insert into later_watchers values (1, '{"carol@example.com","BOB-EXAMPLE"}')`, "later_watchers.watchers (1 rows)"},
		{"a character array", `create table later_pairs (id int primary key, pair character(32)[])`,
			`insert into later_pairs values (1, '{"carol@example.com","Ada-Example"}')`, "later_pairs.pair (1 rows)"},
		{"a citext array", `create extension if not exists citext; create table later_mentions (id int primary key, mentioned citext[])`,
			`insert into later_mentions values (1, '{"Bob-Example"}')`, "later_mentions.mentioned (1 rows)"},
		{"a string deep in jsonb", `create table later_reviews (id int primary key, detail jsonb)`,
			`insert into later_reviews values (1, '{"rounds":[{"by":"ada-example","ok":true}]}')`, "later_reviews.detail (1 rows)"},
		{"a mixed-case jsonb object key below the top level", `create table later_seen (id int primary key, seen jsonb)`,
			`insert into later_seen values (1, '{"meta":{"Bob-Example":{"at":"2026-09-01"}}}')`, "later_seen.seen (1 rows)"},
		{"a json column", `create table later_raw (id int primary key, body json)`,
			`insert into later_raw values (1, '["Ada-Example"]')`, "later_raw.body (1 rows)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			seedEveryPersonField(t, database)
			ctx := context.Background()
			for _, statement := range []string{test.create, test.insert} {
				if _, err := database.Pool.Exec(ctx, statement); err != nil {
					t.Fatalf("%s: %v", statement, err)
				}
			}
			before := databaseSnapshot(t, database)

			out, err := migrate(t, database, examplePeople)
			if err == nil || !strings.Contains(err.Error(), test.hit) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("migrate: err = %v, want a refusal naming %s\n%s", err, test.hit, out)
			}
			column, _, _ := strings.Cut(test.hit, " ")
			if !strings.Contains(out, "migrate-people: census "+column+"=1\n") {
				t.Errorf("output lacks the census line for %s:\n%s", column, out)
			}
			after := databaseSnapshot(t, database)
			for table, rows := range before {
				if after[table] != rows {
					t.Errorf("the refused run changed %s:\nbefore %s\nafter  %s", table, rows, after[table])
				}
			}
		})
	}
}

// A login mentioned inside prose - a comment's body, a sentence in a JSON value, a word in a tag -
// names no one, so the schema census leaves it, and the run succeeds.
func TestRunLeavesALoginMentionedInProse(t *testing.T) {
	database := storetest.Open(t)
	seedEveryPersonField(t, database)
	ctx := context.Background()
	for _, statement := range []string{
		`create table later_notes (id int primary key, body text, detail jsonb, tags text[])`,
		`insert into later_notes values (1, 'ask ada-example about it', '{"text":"Ada-Example wrote this","url":"https://example.com/bob-example"}', '{"ada-example-fan"}')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	out, err := migrate(t, database, examplePeople)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	for _, line := range []string{
		"migrate-people: census later_notes.body=0\n",
		"migrate-people: census later_notes.detail=0\n",
		"migrate-people: census later_notes.tags=0\n",
		"migrate-people: census skips artifact_versions: a document version keeps the names it was written with\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
	var body string
	if err := database.Pool.QueryRow(ctx, `select body from later_notes`).Scan(&body); err != nil || body != "ask ada-example about it" {
		t.Errorf("the prose is %q (%v), want it as written", body, err)
	}
}
