package store

import (
	"context"
	"reflect"
	"strconv"
	"testing"
)

// beforeNodeFacts is the last migration before the node facts: 0130_comments_written_version
// through 0134_asks_written_version_backfill apply after it. writtenVersionTriggers is the last of
// the two triggers (0130, 0131) that the backfills (0133, 0134) run after.
const (
	beforeNodeFacts        = 129
	writtenVersionTriggers = 131
)

// writtenVersion reads the written_version of a comment or an ask: nil when null.
func writtenVersion(t *testing.T, store *Store, table, id string) *int {
	t.Helper()
	var version *int
	if err := store.Pool.QueryRow(context.Background(),
		`select written_version from `+table+` where id = $1`, id).Scan(&version); err != nil {
		t.Fatalf("read %s(%s).written_version: %v", table, id, err)
	}
	return version
}

func assertWrittenVersion(t *testing.T, store *Store, what, table, id string, want *int) {
	t.Helper()
	got := writtenVersion(t, store, table, id)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: written_version = %s, want %s", what, versionText(got), versionText(want))
	}
}

func versionText(version *int) string {
	if version == nil {
		return "null"
	}
	return strconv.Itoa(*version)
}

// seedNodeFactsDocuments seeds project CORE, issue CORE-1 with its spec, and a project document,
// returning the spec's id and the document's.
func seedNodeFactsDocuments(t *testing.T, store *Store) (spec, doc string) {
	t.Helper()
	seedBlockIndexIssue(t, store)
	spec = insertReturningID(t, store, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text`)
	doc = insertReturningID(t, store, `
		insert into artifacts (project_key, slug, name, kind, created_by)
		values ('CORE', 'plan-md', 'plan.md', 'doc', '{"kind":"user","id":"alice"}')
		returning id::text`)
	return spec, doc
}

// Every build's insert records the version an unanchored document comment or ask was written at,
// the earlier build's included, whose statements name no written_version: an unanchored comment
// and an unanchored ask on a project document at version 2 record 2, as does an approval request
// naming version 2 of an issue's spec, which the issue owns. An anchored comment or ask, a block
// ask, an issue's comment and an issue's question record null, and an insert naming a version
// keeps it. Once the document reaches version 3, a new unanchored comment records 3 and the
// first still holds 2.
func TestTheWrittenVersionTriggerRecordsWhatAnyBuildInserts(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	spec, doc := seedNodeFactsDocuments(t, store)
	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown) values
			($1, 1, '# Plan'), ($1, 2, '# Plan v2'), ($2, 1, '# Spec'), ($2, 2, '# Spec v2')
	`, doc, spec); err != nil {
		t.Fatalf("seed versions: %v", err)
	}

	comment := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body) values ($1, '{"kind":"user","id":"alice"}', 'Looks right.')
		returning id::text`, doc)
	ask := insertReturningID(t, store, `
		insert into asks (artifact_id, author, question) values ($1, '{"kind":"session","id":"s1"}', 'Ship it?')
		returning id::text`, doc)
	approval := insertReturningID(t, store, `
		insert into asks (issue_key, author, question, kind, approval)
		values ('CORE-1', '{"kind":"session","id":"s1"}', 'Approve spec.md?', 'approval',
			jsonb_build_object('artifact_id', $1::text, 'name', 'spec.md', 'version', 2))
		returning id::text`, spec)
	anchoredComment := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body, anchor)
		values ($1::text::uuid, '{"kind":"user","id":"alice"}', 'This line.', jsonb_build_object('artifact_id', $1::text, 'version', 1, 'quote', 'Plan'))
		returning id::text`, doc)
	anchoredAsk := insertReturningID(t, store, `
		insert into asks (artifact_id, author, question, anchor)
		values ($1::text::uuid, '{"kind":"session","id":"s1"}', 'This line?', jsonb_build_object('artifact_id', $1::text, 'version', 1, 'quote', 'Plan'))
		returning id::text`, doc)
	blockAsk := insertReturningID(t, store, `
		insert into asks (artifact_id, author, question, block_artifact_id, block_id)
		values ($1, '{"kind":"session","id":"s1"}', 'Decide?', $1, 'decide')
		returning id::text`, doc)
	issueComment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body) values ('CORE-1', '{"kind":"user","id":"alice"}', 'On the issue.')
		returning id::text`)
	issueAsk := insertReturningID(t, store, `
		insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s1"}', 'On the issue?')
		returning id::text`)
	named := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body, written_version) values ($1, '{"kind":"user","id":"alice"}', 'Of v1.', 1)
		returning id::text`, doc)

	assertWrittenVersion(t, store, "an unanchored comment", "comments", comment, new(2))
	assertWrittenVersion(t, store, "an unanchored ask", "asks", ask, new(2))
	assertWrittenVersion(t, store, "an approval request naming version 2", "asks", approval, new(2))
	assertWrittenVersion(t, store, "an anchored comment", "comments", anchoredComment, nil)
	assertWrittenVersion(t, store, "an anchored ask", "asks", anchoredAsk, nil)
	assertWrittenVersion(t, store, "a block ask", "asks", blockAsk, nil)
	assertWrittenVersion(t, store, "an issue's comment", "comments", issueComment, nil)
	assertWrittenVersion(t, store, "an issue's question", "asks", issueAsk, nil)
	assertWrittenVersion(t, store, "a comment naming version 1", "comments", named, new(1))

	if _, err := store.Pool.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown) values ($1, 3, '# Plan v3')`, doc); err != nil {
		t.Fatalf("insert version 3: %v", err)
	}
	later := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body) values ($1, '{"kind":"user","id":"alice"}', 'And now?')
		returning id::text`, doc)
	assertWrittenVersion(t, store, "an unanchored comment at version 3", "comments", later, new(3))
	assertWrittenVersion(t, store, "the first comment after version 3", "comments", comment, new(2))
	assertWrittenVersion(t, store, "the first ask after version 3", "asks", ask, new(2))
}

// The backfills fill the rows stored before the triggers. A project document holds version 1 at
// T1 and version 2 at T2: an unanchored comment and question written between them hold 1, and a
// comment written before T1 keeps null. An approval request whose ask.opened event named version
// 1 holds 1, though its row's approval.version has since moved to 2. A comment the trigger
// recorded before the backfill ran keeps the version it recorded, and an issue's comment keeps
// null.
func TestTheWrittenVersionBackfillFillsOlderRows(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeNodeFacts)
	_, doc := seedNodeFactsDocuments(t, store)
	if _, err := store.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, created_at) values
			($1, 1, '# Plan', '2026-10-01T10:00:00Z'), ($1, 2, '# Plan v2', '2026-10-01T12:00:00Z')
	`, doc); err != nil {
		t.Fatalf("seed versions: %v", err)
	}
	comment := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body, created_at)
		values ($1, '{"kind":"user","id":"alice"}', 'Looks right.', '2026-10-01T11:00:00Z')
		returning id::text`, doc)
	early := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body, created_at)
		values ($1, '{"kind":"user","id":"alice"}', 'Nothing yet.', '2026-10-01T09:00:00Z')
		returning id::text`, doc)
	ask := insertReturningID(t, store, `
		insert into asks (artifact_id, author, question, created_at)
		values ($1, '{"kind":"session","id":"s1"}', 'Ship it?', '2026-10-01T11:00:00Z')
		returning id::text`, doc)
	issueComment := insertReturningID(t, store, `
		insert into comments (issue_key, author, body, created_at)
		values ('CORE-1', '{"kind":"user","id":"alice"}', 'On the issue.', '2026-10-01T11:00:00Z')
		returning id::text`)
	approval := insertReturningID(t, store, `
		insert into asks (artifact_id, author, question, kind, approval, created_at)
		values ($1::text::uuid, '{"kind":"session","id":"s1"}', 'Approve plan.md?', 'approval',
			jsonb_build_object('artifact_id', $1::text, 'name', 'plan.md', 'version', 2, 'requested_version', 1),
			'2026-10-01T11:00:00Z')
		returning id::text`, doc)
	if _, err := store.Pool.Exec(ctx, `
		insert into events (artifact_id, seq, type, actor, payload, notify) values
			($1::text::uuid, 1, 'ask.opened', '{"kind":"session","id":"s1"}',
				jsonb_build_object('id', $2::text, 'kind', 'approval',
					'approval', jsonb_build_object('artifact_id', $1::text, 'name', 'plan.md', 'version', 1, 'requested_version', 1)), true),
			($1::text::uuid, 2, 'ask.edited', '{"kind":"session","id":"s1"}',
				jsonb_build_object('id', $2::text, 'kind', 'approval',
					'approval', jsonb_build_object('artifact_id', $1::text, 'name', 'plan.md', 'version', 2, 'requested_version', 1)), true)
	`, doc, approval); err != nil {
		t.Fatalf("seed the approval request's events: %v", err)
	}

	// A comment written once the triggers exist, before the backfill runs, is recorded by its
	// trigger at the document's latest version, which the backfill's reading by time would not give.
	migrateThrough(t, store, writtenVersionTriggers)
	raced := insertReturningID(t, store, `
		insert into comments (artifact_id, author, body, created_at)
		values ($1, '{"kind":"user","id":"alice"}', 'Written during the roll.', '2026-10-01T11:00:00Z')
		returning id::text`, doc)
	assertWrittenVersion(t, store, "a comment the trigger recorded", "comments", raced, new(2))

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertWrittenVersion(t, store, "a comment written between versions 1 and 2", "comments", comment, new(1))
	assertWrittenVersion(t, store, "a comment written before version 1", "comments", early, nil)
	assertWrittenVersion(t, store, "a question written between versions 1 and 2", "asks", ask, new(1))
	assertWrittenVersion(t, store, "an approval request opened at version 1", "asks", approval, new(1))
	assertWrittenVersion(t, store, "a comment the trigger recorded, after the backfill", "comments", raced, new(2))
	assertWrittenVersion(t, store, "an issue's comment", "comments", issueComment, nil)
}

// Every component stored before component_facts has a row, with made_at, changed_at and digest
// null: when it was made or last changed was never recorded.
func TestComponentFactsSeedEveryComponentUnrecorded(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, beforeNodeFacts)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core'), ('OPS', 'Operations');
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
			values ('CORE', 'acme/arch', 'main', 42, '{"kind":"user","id":"alice"}'),
			       ('OPS', 'acme/ops', 'main', 42, '{"kind":"user","id":"alice"}');
		insert into architecture_snapshots (id, project_key, commit, files)
			values (500, 'CORE', 'commit-one', '{}'), (501, 'OPS', 'commit-one', '{}');
		insert into components (project_key, id, title, prose, parent, snapshot_id, source_id) values
			('CORE', 'platform', 'Platform', 'The platform.', null, 500,
				(select id from architecture_sources where project_key = 'CORE')),
			('CORE', 'web', 'Web', 'The web app.', 'platform', 500,
				(select id from architecture_sources where project_key = 'CORE')),
			('OPS', 'deploy', 'Deploy', 'The deploy.', null, 501,
				(select id from architecture_sources where project_key = 'OPS'));
	`); err != nil {
		t.Fatalf("seed components: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := store.Pool.Query(ctx, `
		select project_key || '/' || id, made_at is null and changed_at is null and digest is null
		from component_facts order by project_key, id`)
	if err != nil {
		t.Fatalf("read component_facts: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var node string
		var unrecorded bool
		if err := rows.Scan(&node, &unrecorded); err != nil {
			t.Fatalf("scan component_facts: %v", err)
		}
		got[node] = unrecorded
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate component_facts: %v", err)
	}
	want := map[string]bool{"CORE/platform": true, "CORE/web": true, "OPS/deploy": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("component_facts (node: all three null) = %v, want %v", got, want)
	}
}
