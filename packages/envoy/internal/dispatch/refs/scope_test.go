package refs

import (
	"cmp"
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// insertScopeRow runs an insert returning one id and answers it as text.
func insertScopeRow(t *testing.T, tx pgx.Tx, what, sql string, args ...any) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert %s: %v", what, err)
	}
	return id
}

func insertScopeDocument(t *testing.T, tx pgx.Tx, issueKey, slug string, primary bool) string {
	t.Helper()
	return insertScopeRow(t, tx, "document "+slug, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, 'ACME', $2, $2, 'doc', $3, '{"kind":"user","id":"alice"}')
		returning id::text
	`, issueKey, slug, primary)
}

func insertScopeVersion(t *testing.T, tx pgx.Tx, artifactID string, number int, markdown string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown) values ($1, $2, $3)
	`, artifactID, number, markdown); err != nil {
		t.Fatalf("insert version %d of %s: %v", number, artifactID, err)
	}
}

func insertScopeAsk(t *testing.T, tx pgx.Tx, issueKey any, column string, value any) string {
	t.Helper()
	sql := `insert into asks (issue_key, author, question) values ($1, '{"kind":"user","id":"alice"}', 'Which?') returning id::text`
	args := []any{issueKey}
	switch column {
	case "anchor":
		sql = `insert into asks (issue_key, author, question, anchor) values ($1, '{"kind":"user","id":"alice"}', 'Which?', jsonb_build_object('artifact_id', $2::text, 'mark_id', 'm1')) returning id::text`
		args = append(args, value)
	case "artifact_id":
		sql = `insert into asks (issue_key, author, question, artifact_id) values ($1, '{"kind":"user","id":"alice"}', 'Which?', $2) returning id::text`
		args = append(args, value)
	case "":
	default:
		t.Fatalf("unknown ask column %q", column)
	}
	return insertScopeRow(t, tx, "ask", sql, args...)
}

func insertScopeBlockAsk(t *testing.T, tx pgx.Tx, issueKey any, artifactID, blockID string) string {
	t.Helper()
	// An ask has one owner: its issue, or for a project document's ask that document.
	return insertScopeRow(t, tx, "block ask "+blockID, `
		insert into asks (issue_key, artifact_id, author, question, block_id, block_artifact_id)
		values ($1, case when $1::text is null then $3::uuid end, '{"kind":"user","id":"alice"}', 'Which?', $2, $3)
		returning id::text
	`, issueKey, blockID, artifactID)
}

// One fixture of every shape ScopeOf names, each with the document it belongs to; ScopeSources of
// each document lists exactly the sources ScopeOf puts in it, and a direct message, like a reply
// whose thread loops back on itself before naming a document, belongs to none.
func TestScopeOfAndScopeSourcesAgree(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	notes := insertScopeDocument(t, tx, "ACME-1", "notes", false)
	project, _ := createReferenceArtifact(t, tx, "", "ACME", "handbook")

	blockAsk := insertScopeBlockAsk(t, tx, "ACME-1", notes, "q1")
	anchoredAsk := insertScopeAsk(t, tx, "ACME-1", "anchor", notes)
	issueAsk := insertScopeAsk(t, tx, "ACME-1", "", nil)
	projectAsk := insertScopeAsk(t, tx, nil, "artifact_id", project)
	anchoredComment := insertScopeRow(t, tx, "anchored comment", `
		insert into comments (issue_key, author, body, anchor)
		values ('ACME-1', '{"kind":"user","id":"alice"}', 'anchored', jsonb_build_object('artifact_id', $1::text, 'mark_id', 'm2'))
		returning id::text
	`, notes)
	ownerComment := insertScopeRow(t, tx, "owner-document comment", `
		insert into comments (artifact_id, author, body) values ($1, '{"kind":"user","id":"alice"}', 'on the handbook')
		returning id::text
	`, project)
	askReply := insertScopeRow(t, tx, "ask reply", `
		insert into comments (issue_key, author, body, ask_id) values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1)
		returning id::text
	`, blockAsk)
	issueAskReply := insertScopeRow(t, tx, "issue ask reply", `
		insert into comments (issue_key, author, body, ask_id) values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1)
		returning id::text
	`, issueAsk)
	threadReply := insertScopeRow(t, tx, "thread reply", `
		insert into comments (issue_key, author, body, reply_to) values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1)
		returning id::text
	`, anchoredComment)
	nestedReply := insertScopeRow(t, tx, "nested thread reply", `
		insert into comments (issue_key, author, body, reply_to) values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1)
		returning id::text
	`, threadReply)
	issueComment := createReferenceComment(t, tx, "ACME-1")
	issueThreadReply := insertScopeRow(t, tx, "issue thread reply", `
		insert into comments (issue_key, author, body, reply_to) values ('ACME-1', '{"kind":"user","id":"alice"}', 'reply', $1)
		returning id::text
	`, issueComment)
	loopingReply := insertScopeRow(t, tx, "looping reply", `
		insert into comments (id, issue_key, author, body, reply_to)
		values ('6f1c1f39-3d43-4b8e-9d0a-2f0d6c5b7a10', 'ACME-1', '{"kind":"user","id":"alice"}', 'loop', '6f1c1f39-3d43-4b8e-9d0a-2f0d6c5b7a10')
		returning id::text
	`)
	issueMessage := insertScopeRow(t, tx, "issue message", `
		insert into messages (issue_key, author, body) values ('ACME-1', '{"kind":"session","id":"s1"}', 'hello')
		returning id::text
	`)
	directMessage := insertScopeRow(t, tx, "direct message", `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'hello', 'session:s1')
		returning id::text
	`)

	shapes := []struct {
		name string
		Target
		scope string
	}{
		{"the spec's version", Target{"artifact", spec}, spec},
		{"another document's version", Target{"artifact", notes}, notes},
		{"a project document's version", Target{"artifact", project}, project},
		{"a block ask", Target{"ask", blockAsk}, notes},
		{"an anchored ask", Target{"ask", anchoredAsk}, notes},
		{"an issue ask", Target{"ask", issueAsk}, spec},
		{"a project document's ask", Target{"ask", projectAsk}, project},
		{"an anchored comment", Target{"comment", anchoredComment}, notes},
		{"an owner-document comment", Target{"comment", ownerComment}, project},
		{"a reply to a block ask", Target{"comment", askReply}, notes},
		{"a reply to an issue ask", Target{"comment", issueAskReply}, spec},
		{"a thread reply", Target{"comment", threadReply}, notes},
		{"a reply to a thread reply", Target{"comment", nestedReply}, notes},
		{"an issue comment", Target{"comment", issueComment}, spec},
		{"a reply to an issue comment", Target{"comment", issueThreadReply}, spec},
		{"a reply looping on itself", Target{"comment", loopingReply}, ""},
		{"an issue message", Target{"message", issueMessage}, spec},
		{"a direct message", Target{"message", directMessage}, ""},
	}
	want := map[string][]Target{spec: {}, notes: {}, project: {}}
	for _, shape := range shapes {
		got, err := ScopeOf(ctx, tx, shape.Kind, shape.ID)
		if err != nil {
			t.Fatalf("scope of %s: %v", shape.name, err)
		}
		if got != shape.scope {
			t.Errorf("scope of %s = %q; want %q", shape.name, got, shape.scope)
		}
		if shape.scope != "" {
			want[shape.scope] = append(want[shape.scope], shape.Target)
		}
	}
	for doc, sources := range want {
		slices.SortFunc(sources, func(a, b Target) int {
			return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ID, b.ID))
		})
		got, err := ScopeSources(ctx, tx, doc)
		if err != nil {
			t.Fatalf("scope sources of %s: %v", doc, err)
		}
		if !reflect.DeepEqual(got, sources) {
			t.Errorf("scope sources of %s = %v; want %v", doc, got, sources)
		}
	}
	if got, err := ScopeOf(ctx, tx, "comment", "c1"); err != nil || got != "" {
		t.Fatalf("scope of a source that is not a uuid = %q, %v; want none", got, err)
	}
}

// A document's question map is the question blocks its latest version declares: a block settlement
// made a row for, a copied block under the ask it was copied from, and a block in a quote; never a
// block not settled yet, one whose copied_from names no ask, an id an older version alone declares,
// or a `#` inside an attribute's value. Each maps to the reference text.Extract reads from that
// ask's dispatch:// address.
func TestQuestionMapReadsTheLatestVersionsQuestionBlocks(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	record := insertScopeDocument(t, tx, "ACME-1", "record", false)
	handbook, _ := createReferenceArtifact(t, tx, "", "ACME", "handbook")

	q1 := insertScopeBlockAsk(t, tx, "ACME-1", spec, "q1")
	quoted := insertScopeBlockAsk(t, tx, "ACME-1", spec, "quoted")
	insertScopeBlockAsk(t, tx, "ACME-1", spec, "gone")
	source := insertScopeBlockAsk(t, tx, "ACME-1", record, "q2")
	projectQuestion := insertScopeBlockAsk(t, tx, nil, handbook, "rule")

	insertScopeVersion(t, tx, spec, 1, ":::ask{#gone}\nGone?\n:::\n")
	insertScopeVersion(t, tx, spec, 2, "# Spec\n\n"+
		`:::ask{#q1 urgency="med" answer="see #fake and \"#q9\"" state="answered"}`+"\nWhich?\n:::\n\n"+
		`:::ask{#q2 state="open" copied_from="`+source+`" copied_from_document="dispatch://ACME-1/artifact/record"}`+"\nWhich?\n:::\n\n"+
		"> :::ask{#quoted}\n> Which?\n> :::\n\n"+
		":::ask{#unsettled}\nWhich?\n:::\n\n"+
		`:::ask{#forged copied_from="00000000-0000-4000-8000-000000000000"}`+"\nWhich?\n:::\n\n"+
		"```\n:::ask{#fake}\n```\n")
	insertScopeVersion(t, tx, handbook, 1, ":::ask{#rule}\nWhich?\n:::\n")

	issueAsk := func(id string) text.Ref { return text.Extract("dispatch://ACME-1/ask/"+id, "")[0] }
	for doc, want := range map[string]map[string]text.Ref{
		spec:     {"q1": issueAsk(q1), "q2": issueAsk(source), "quoted": issueAsk(quoted)},
		record:   nil,
		handbook: {"rule": text.Extract("dispatch://ACME/artifact/handbook/ask/"+projectQuestion, "")[0]},
	} {
		got, err := QuestionMap(ctx, tx, doc)
		if err != nil {
			t.Fatalf("question map of %s: %v", doc, err)
		}
		if len(got) != len(want) || (len(want) > 0 && !reflect.DeepEqual(got, want)) {
			t.Errorf("question map of %s = %#v; want %#v", doc, got, want)
		}
	}
}

// A bare key is stored only when its issue exists and a `#id` only as the question its source's
// document declares under that id, both as the dispatch:// address of the target is; a key-shaped
// word with no issue, a key inside a dashboard address, and a `#id` from a direct message's text
// store nothing more.
func TestReplaceStoresTheBareKeysAndQuestionIDsThatResolve(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('ACME-2', 'ACME', 2, 'Second', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create ACME-2: %v", err)
	}
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	q2 := insertScopeBlockAsk(t, tx, "ACME-1", spec, "q2")
	insertScopeVersion(t, tx, spec, 1, ":::ask{#q2}\nWhich?\n:::\n")
	comment := createReferenceComment(t, tx, "ACME-1")
	direct := insertScopeRow(t, tx, "direct message", `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'hello', 'session:s1')
		returning id::text
	`)

	for _, tc := range []struct {
		kind, id, body string
		want           []Target
	}{
		{"comment", comment, "blocked on ACME-2, see _#q2_; not ACME-999999, UTF-8 or #nope; https://dispatch.example/issues/ACME-2", []Target{{"ask", q2}, {"issue", "ACME-2"}}},
		{"message", direct, "ACME-2 and #q2", []Target{{"issue", "ACME-2"}}},
	} {
		if _, err := replace(ctx, tx, tc.kind, tc.id, tc.body, "https://dispatch.example"); err != nil {
			t.Fatalf("replace %s: %v", tc.kind, err)
		}
		got, err := scanTargets(tx.Query(ctx, `select to_kind, to_id from refs where from_kind = $1 and from_id = $2 order by to_kind, to_id`, tc.kind, tc.id))
		if err != nil {
			t.Fatalf("read %s references: %v", tc.kind, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s references = %v; want %v", tc.kind, got, tc.want)
		}
	}
}

// A bare key's and a `#id`'s excerpt is the block it is written in, as a dispatch:// address's is.
func TestMentionExcerptsCutBareKeysAndQuestionIDsWhereTheyAreWritten(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	createReferenceProject(t, tx, "ACME")
	createReferenceIssue(t, tx, "ACME-1", "ACME")
	spec := insertScopeDocument(t, tx, "ACME-1", "spec", true)
	q2 := insertScopeBlockAsk(t, tx, "ACME-1", spec, "q2")
	markdown := "# Spec\n\nBlocked on ACME-1.\n\nSee #q2 first.\n\n:::ask{#q2}\nWhich?\n:::\n"
	insertScopeVersion(t, tx, spec, 1, markdown)
	blocks := []model.ArtifactBlock{
		{ID: "b-head", From: 0, To: 6},
		{ID: "b-key", From: 8, To: 26},
		{ID: "b-id", From: 28, To: 42},
	}
	got, err := MentionExcerpts(markdown, blocks, "https://dispatch.example", NewResolver(ctx, tx, "artifact", spec))
	if err != nil {
		t.Fatalf("mention excerpts: %v", err)
	}
	want := map[Target]model.GraphExcerpt{
		{"issue", "ACME-1"}: {BlockID: "b-key", Text: "Blocked on ACME-1."},
		{"ask", q2}:         {BlockID: "b-id", Text: "See #q2 first."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mention excerpts = %#v; want %#v", got, want)
	}
}
