package peoplemigration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

const (
	ada = "ada@example.com"
	bob = "bob@example.com"
)

// examplePeople is the map an operator hands the migration: each GitHub login Dispatch knew a
// person by, lowercased, to that person's email.
var examplePeople = Map{"ada-example": ada, "bob-example": bob}

// specMarkdown is CORE-1's spec: one ask Ada answered under her GitHub login, in the display
// casing the GitHub sign-in recorded.
const specMarkdown = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"answered\" answered_by=\"Ada-Example\" answered_at=\"2026-09-15T17:37:34Z\" selected=\"[&#x22;REST&#x22;]\"}\nWhich transport?\n:::\n"

// notesMarkdown is CORE-2's notes: one ask Bob answered under his GitHub login.
const notesMarkdown = ":::ask{#when urgency=\"low\" multiple=\"false\" state=\"answered\" answered_by=\"Bob-Example\" answered_at=\"2026-09-16T10:00:00Z\" selected=\"[&#x22;Later&#x22;]\"}\nWhen?\n:::\n"

// seedEveryPersonField writes a database as Dispatch left it under GitHub sign-in: every field
// that names a person, holding the login in the casings its writers stored (the display casing
// the sign-in returned, and the lowercase the per-person tables canonicalise to), beside values
// that are not logins and stay as they are. It returns CORE-1's spec document.
func seedEveryPersonField(t *testing.T, database *store.Store) string {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", strings.Fields(sql)[2], err)
		}
	}
	exec(`insert into projects (key, name) values ('CORE', 'Core')`)
	exec(`
		insert into issues (key, project_key, number, title, rank, created_by, assignee, claimed_by, claimed_at) values
		('CORE-1', 'CORE', 1, 'One', 'U', '{"kind":"user","id":"Ada-Example"}', 'ada-example', '{"kind":"session","id":"s1","owner":"Ada-Example"}', now()),
		('CORE-2', 'CORE', 2, 'Two', 'V', '{"kind":"session","id":"s2"}', 'bob-example', null, null),
		('CORE-3', 'CORE', 3, 'Three', 'W', '{"kind":"user","id":"carol@example.com"}', null, null, null)
	`)
	var spec string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-1', 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"Ada-Example"}')
		returning id::text
	`).Scan(&spec); err != nil {
		t.Fatalf("seed artifacts: %v", err)
	}
	seedDocument(t, database, spec, specMarkdown)
	exec(`
		insert into asks (issue_key, block_id, block_artifact_id, author, question, options, multiple, urgency, state, answer)
		values ('CORE-1', 'decision', $1, '{"kind":"user","id":"Ada-Example"}', 'Which transport?', '[]', false, 'high', 'answered',
			'{"user":"Ada-Example","selected":["REST"],"text":null,"at":"2026-09-15T17:37:34Z"}')
	`, spec)
	exec(`
		insert into asks (issue_key, author, question, state, resolution) values
		('CORE-2', '{"kind":"session","id":"s2","owner":"bob-example"}', 'Moot?', 'resolved',
			'{"kind":"retracted","reason":"moot","actor":{"kind":"user","id":"bob-example"},"at":"2026-09-16T00:00:00Z"}')
	`)
	exec(`
		insert into comments (issue_key, author, body, resolved, resolved_by, resolved_at)
		values ('CORE-1', '{"kind":"user","id":"Ada-Example"}', 'a note', true, '{"kind":"user","id":"bob-example"}', now())
	`)
	var broadcast, direct string
	if err := database.Pool.QueryRow(ctx, `
		insert into broadcasts (author, body, delivery) values ('{"kind":"user","id":"Ada-Example"}', 'to everyone', 'steer')
		returning id::text
	`).Scan(&broadcast); err != nil {
		t.Fatalf("seed broadcasts: %v", err)
	}
	exec(`insert into messages (issue_key, author, body) values ('CORE-1', '{"kind":"session","id":"s1","owner":"bob-example"}', 'on the issue')`)
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (target, author, body, broadcast_id) values ('session:s1', '{"kind":"user","id":"Ada-Example"}', 'direct', $1)
		returning id::text
	`, broadcast).Scan(&direct); err != nil {
		t.Fatalf("seed messages: %v", err)
	}
	exec(`
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by)
		values ($1, 1, 'steer', 's1', 'sent', '{"kind":"user","id":"Ada-Example"}')
	`, direct)
	exec(`
		insert into events (issue_key, project_key, seq, type, actor, payload, notify) values
		('CORE-1', null, 1, 'issue.created', '{"kind":"user","id":"Ada-Example"}',
			'{"key":"CORE-1","assignee":"ada-example","created_by":{"kind":"user","id":"Ada-Example"},"claim":{"actor":{"kind":"session","id":"s1","owner":"Ada-Example"},"at":"2026-09-15T00:00:00Z"},"priority":2}', true),
		('CORE-1', null, 2, 'ask.answered', '{"kind":"user","id":"Ada-Example"}',
			'{"question":"Which transport?","author":{"kind":"user","id":"Ada-Example"},"answer":{"user":"Ada-Example","selected":["REST"],"text":"<b>REST</b> & more"}}', true),
		('CORE-1', null, 3, 'ask.settled', '{"kind":"system","id":"document-settlement"}', '{"id":"decision","kind":"question"}', true),
		(null, 'CORE', 1, 'user_state.updated', '{"kind":"user","id":"bob-example"}', '{"login":"bob-example","state":{"pinned":true}}', false),
		(null, null, 1, 'user_agent_state.updated', '{"kind":"user","id":"Ada-Example"}', '{"login":"Ada-Example","session_id":"s1"}', false)
	`)
	exec(`insert into artifact_reviews (artifact_id, version, state, actor) values ($1, 1, 'approved', '{"kind":"user","id":"Ada-Example"}')`, spec)
	exec(`insert into repo_projects (repo, project, created_by) values ('example/repo', 'CORE', '{"kind":"user","id":"Ada-Example"}')`)
	exec(`
		insert into architecture_sources (project_key, repo, branch, installation_id, created_by)
		values ('CORE', 'example/repo', 'main', 1, '{"kind":"user","id":"bob-example"}')
	`)
	exec(`insert into agent_tokens (owner, name, token_hash, prefix) values ('Ada-Example', 'laptop', '\x01', 'dsp_ada')`)
	exec(`
		insert into user_issue_state (login, issue_key, pinned, last_read_seq, dismissed, seq) values
		('Ada-Example', 'CORE-1', true, 3, '["a"]', 5),
		('ada-example', 'CORE-1', false, 7, '["b"]', 2),
		('ada@example.com', 'CORE-2', false, 1, '[]', 1),
		('ada-example', 'CORE-2', true, 0, '["c"]', 4),
		('bob-example', 'CORE-3', false, 2, '["z","y"]', 0)
	`)
	exec(`
		insert into user_agent_state (login, session_id, cleared_before) values
		('Ada-Example', 's1', '2026-09-01T00:00:00Z'), ('ada-example', 's1', '2026-09-02T00:00:00Z')
	`)
	exec(`insert into user_agent_read (login, session_id, read_through) values ('ada-example', 's1', '2026-09-03T00:00:00Z')`)
	exec(`
		insert into user_ask_snooze (login, ask_id, snoozed_until)
		select 'Ada-Example', id, '2026-10-01T00:00:00Z' from asks where block_id = 'decision'
	`)
	exec(`insert into broadcast_idempotency_keys (login, idempotency_key, broadcast_id, request_digest) values ('ada-example', 'k1', $1, 'digest')`, broadcast)
	exec(`insert into user_sessions (login, generation) values ('Ada-Example', 3), ('ada@example.com', 1)`)
	return spec
}

// seedDocument writes a document's live state and its first version as a server write leaves
// them: the version records the state's update, so settling the document versions nothing new.
func seedDocument(t *testing.T, database *store.Store, artifactID, markdown string) {
	t.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	pmdoc.EnsureBlockIDsCount(tree)
	rendered, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		return pmdoc.Update(transaction, fragment, tree)
	}); err != nil {
		t.Fatalf("write the document's state: %v", err)
	}
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the document: %v", err)
	}
	defer tx.Rollback(ctx)
	version, err := docs.NewPgVersioned(database).AppendUpdateTx(ctx, tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil), true)
	if err != nil {
		t.Fatalf("append the document's state: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors, doc_update_version)
		values ($1, 1, $2, '[{"kind":"user","id":"Ada-Example"}]', $3)
	`, artifactID, rendered, int64(version)); err != nil {
		t.Fatalf("seed the document's version: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the document: %v", err)
	}
}

// migrate runs the migration as `envoy-dispatch migrate-people` does: on a document service of
// its own, shut down once the run returns.
func migrate(t *testing.T, database *store.Store, people Map) (string, error) {
	t.Helper()
	documents := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	var out bytes.Buffer
	err := Run(context.Background(), database, documents, people, &out)
	if shutdownErr := documents.Shutdown(context.Background()); shutdownErr != nil {
		t.Fatalf("shut down the migration's document service: %v", shutdownErr)
	}
	return out.String(), err
}

// databaseSnapshot is every table's rows, so a test can tell whether a run wrote anything.
func databaseSnapshot(t *testing.T, database *store.Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := database.Pool.Query(ctx, `
		select table_name from information_schema.tables
		where table_schema = current_schema() and table_type = 'BASE TABLE'
		order by table_name
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, table)
	}
	rows.Close()
	snapshot := make(map[string]string, len(tables))
	for _, table := range tables {
		var contents string
		if err := database.Pool.QueryRow(ctx, `
			select coalesce(jsonb_agg(to_jsonb(t) order by to_jsonb(t)::text), '[]')::text from `+table+` t
		`).Scan(&contents); err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		snapshot[table] = contents
	}
	return snapshot
}

// loginHolders names every table outside a document's history that still holds login anywhere in
// a row. artifact_versions keeps the name each version was written with, and doc_updates,
// doc_snapshots and doc_checkpoints are the documents' Yjs history, which a rename appends to.
func loginHolders(t *testing.T, database *store.Store, login string) []string {
	t.Helper()
	var holders []string
	for table := range databaseSnapshot(t, database) {
		switch table {
		case "artifact_versions", "doc_updates", "doc_snapshots", "doc_checkpoints":
			continue
		}
		var count int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from `+table+` t where to_jsonb(t)::text ilike '%' || $1 || '%'`, login,
		).Scan(&count); err != nil {
			t.Fatalf("search %s for %s: %v", table, login, err)
		}
		if count > 0 {
			holders = append(holders, table)
		}
	}
	return holders
}

func readJSON(t *testing.T, database *store.Store, query string, args ...any) any {
	t.Helper()
	var raw []byte
	if err := database.Pool.QueryRow(context.Background(), query, args...).Scan(&raw); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %q: %v", query, err)
	}
	return value
}

// Every field that names a person moves from the login to the email, whatever casing the login
// was stored in; rows two casings of one login kept apart become that person's one row; every
// value that is not a login is left as it was; a document's answered asks name the email and
// the document still renders; the versions a document was written as keep their names; and the
// run reports each field's count of logins before and after.
func TestRunMovesEveryPersonFieldToEmail(t *testing.T) {
	database := storetest.Open(t)
	spec := seedEveryPersonField(t, database)
	versionsBefore := readJSON(t, database, `select jsonb_agg(to_jsonb(v) order by number) from artifact_versions v`)

	out, err := migrate(t, database, examplePeople)
	if err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}

	for _, login := range []string{"ada-example", "bob-example"} {
		if holders := loginHolders(t, database, login); len(holders) != 0 {
			t.Errorf("tables still holding %s: %v\n%s", login, holders, out)
		}
	}

	for _, check := range []struct {
		name, query string
		want        any
	}{
		{"CORE-1's creator", `select created_by from issues where key = 'CORE-1'`, map[string]any{"kind": "user", "id": ada}},
		{"CORE-1's claim, a session Ada's token wrote", `select claimed_by from issues where key = 'CORE-1'`,
			map[string]any{"kind": "session", "id": "s1", "owner": ada}},
		{"a session that names no person", `select created_by from issues where key = 'CORE-2'`, map[string]any{"kind": "session", "id": "s2"}},
		{"a person already named by email", `select created_by from issues where key = 'CORE-3'`, map[string]any{"kind": "user", "id": "carol@example.com"}},
		{"the assignees", `select jsonb_object_agg(key, assignee) from issues where assignee is not null`,
			map[string]any{"CORE-1": ada, "CORE-2": bob}},
		{"the answer's user", `select answer from asks where block_id = 'decision'`,
			map[string]any{"user": ada, "selected": []any{"REST"}, "text": nil, "at": "2026-09-15T17:37:34Z"}},
		{"the resolution's actor", `select resolution->'actor' from asks where state = 'resolved'`, map[string]any{"kind": "user", "id": bob}},
		{"the issue event's payload", `select payload from events where type = 'issue.created'`, map[string]any{
			"key": "CORE-1", "assignee": ada, "created_by": map[string]any{"kind": "user", "id": ada}, "priority": 2.0,
			"claim": map[string]any{"actor": map[string]any{"kind": "session", "id": "s1", "owner": ada}, "at": "2026-09-15T00:00:00Z"},
		}},
		{"the answer event's payload, its text kept", `select payload from events where type = 'ask.answered'`, map[string]any{
			"question": "Which transport?", "author": map[string]any{"kind": "user", "id": ada},
			"answer": map[string]any{"user": ada, "selected": []any{"REST"}, "text": "<b>REST</b> & more"},
		}},
		{"a system actor's event", `select jsonb_build_array(actor, payload) from events where type = 'ask.settled'`,
			[]any{map[string]any{"kind": "system", "id": "document-settlement"}, map[string]any{"id": "decision", "kind": "question"}}},
		{"the per-person bookkeeping events", `select jsonb_agg(payload->'login' order by type) from events where payload ? 'login'`,
			[]any{ada, bob}},
		{"the token's owner", `select to_jsonb(owner) from agent_tokens`, ada},
		{"Ada's issue state on CORE-1, from two casings", `select to_jsonb(s) - 'issue_key' from user_issue_state s where login = $1 and issue_key = 'CORE-1'`,
			map[string]any{"login": ada, "pinned": true, "last_read_seq": 7.0, "dismissed": []any{"a", "b"}, "seq": 5.0}},
		{"Ada's issue state on CORE-2, merged into the row her email already had", `select to_jsonb(s) - 'issue_key' from user_issue_state s where login = $1 and issue_key = 'CORE-2'`,
			map[string]any{"login": ada, "pinned": true, "last_read_seq": 1.0, "dismissed": []any{"c"}, "seq": 4.0}},
		{"Bob's issue state, its dismissals in their order", `select dismissed from user_issue_state where login = 'bob@example.com'`, []any{"z", "y"}},
		{"Ada's Clear, the later of her two", `select jsonb_agg(to_jsonb(s)) from user_agent_state s`,
			[]any{map[string]any{"login": ada, "session_id": "s1", "cleared_before": "2026-09-02T00:00:00+00:00"}}},
		{"the session generations: the login's goes, the email's stays", `select jsonb_object_agg(login, generation) from user_sessions`,
			map[string]any{ada: 1.0}},
		{"the people the records name", `select jsonb_agg(email order by email) from people`, []any{ada, bob}},
	} {
		args := []any{}
		if strings.Contains(check.query, "$1") {
			args = append(args, ada)
		}
		if got := readJSON(t, database, check.query, args...); !reflect.DeepEqual(got, check.want) {
			t.Errorf("%s = %#v, want %#v", check.name, got, check.want)
		}
	}

	if got := readJSON(t, database, `select jsonb_agg(to_jsonb(v) order by number) from artifact_versions v`); !reflect.DeepEqual(got, versionsBefore) {
		t.Errorf("the spec's versions changed: %#v, want %#v", got, versionsBefore)
	}
	if got, want := documentText(t, database, spec), strings.Replace(specMarkdown, "Ada-Example", ada, 1); got != want {
		t.Errorf("the spec renders %q, want %q", got, want)
	}

	for _, line := range []string{
		"migrate-people: before issues.assignee=2\n",
		"migrate-people: before user_issue_state.login=4\n",
		"migrate-people: before events.payload=4\n",
		"migrate-people: before user_sessions.login=1\n",
		"migrate-people: before documents.answered_by=1\n",
		"migrate-people: after issues.assignee=0\n",
		"migrate-people: after documents.answered_by=0\n",
		"migrate-people: people recorded=2\n",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "migrate-people: after ") && !strings.HasSuffix(line, "=0") {
			t.Errorf("a field still counts logins after the run: %q", line)
		}
	}
}

// A run over a database already moved finds no login and writes nothing.
func TestRunASecondTimeChangesNothing(t *testing.T) {
	database := storetest.Open(t)
	seedEveryPersonField(t, database)
	if out, err := migrate(t, database, examplePeople); err != nil {
		t.Fatalf("first run: %v\n%s", err, out)
	}
	before := databaseSnapshot(t, database)

	out, err := migrate(t, database, examplePeople)
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out)
	}
	after := databaseSnapshot(t, database)
	for table, rows := range before {
		if after[table] != rows {
			t.Errorf("the second run changed %s:\nbefore %s\nafter  %s", table, rows, after[table])
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "migrate-people: before ") && !strings.HasSuffix(line, "=0") {
			t.Errorf("the second run found a login: %q", line)
		}
	}
	if !strings.Contains(out, "migrate-people: people recorded=0\n") {
		t.Errorf("the second run recorded people:\n%s", out)
	}
}

// A login the map has no email for stops the run before it writes anything, and the refusal names
// the login and every field holding it, so the operator can add it to the map.
func TestRunRefusesALoginTheMapLacks(t *testing.T) {
	database := storetest.Open(t)
	seedEveryPersonField(t, database)
	before := databaseSnapshot(t, database)

	out, err := migrate(t, database, Map{"ada-example": ada})
	if err == nil {
		t.Fatalf("migrate with bob-example unmapped succeeded:\n%s", out)
	}
	for _, want := range []string{"bob-example", "issues.assignee", "asks.resolution", "events.payload", "architecture_sources.created_by", "user_issue_state.login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "ada-example") {
		t.Errorf("refusal %q names a login the map has", err)
	}
	if !strings.Contains(out, "migrate-people: before issues.assignee=2\n") {
		t.Errorf("a refused run does not report what it found:\n%s", out)
	}
	after := databaseSnapshot(t, database)
	for table, rows := range before {
		if after[table] != rows {
			t.Errorf("the refused run changed %s:\nbefore %s\nafter  %s", table, rows, after[table])
		}
	}
}

// A document whose state cannot be read holds nothing else back: the database moves, the run
// names that document and exits non-zero, and once the document can be read a second run renames
// the asks answered in it.
func TestRunMovesTheDatabaseAndNamesADocumentItCouldNotRead(t *testing.T) {
	database := storetest.Open(t)
	spec := seedEveryPersonField(t, database)
	ctx := context.Background()
	var notes string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('CORE-2', 'CORE', 'notes', 'notes.md', 'doc', true, '{"kind":"user","id":"Bob-Example"}')
		returning id::text
	`).Scan(&notes); err != nil {
		t.Fatalf("seed the notes: %v", err)
	}
	seedDocument(t, database, notes, notesMarkdown)
	if _, err := database.Pool.Exec(ctx, `
		insert into asks (issue_key, block_id, block_artifact_id, author, question, options, multiple, urgency, state, answer)
		values ('CORE-2', 'when', $1, '{"kind":"user","id":"Bob-Example"}', 'When?', '[]', false, 'low', 'answered',
			'{"user":"Bob-Example","selected":["Later"],"text":null,"at":"2026-09-16T10:00:00Z"}')
	`, notes); err != nil {
		t.Fatalf("seed the notes' ask: %v", err)
	}
	// The notes' state gains an update no reader can decode.
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed)
		select $1, max(version) + 1, $2, false from doc_updates where artifact_id = $1
	`, notes, []byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("corrupt the notes' state: %v", err)
	}

	out, err := migrate(t, database, examplePeople)
	if err == nil {
		t.Fatalf("a run that could not read the notes succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), notes) {
		t.Errorf("the run's error %q does not name the notes %s", err, notes)
	}
	if strings.Contains(err.Error(), spec) {
		t.Errorf("the run's error %q names the spec %s, which it renamed", err, spec)
	}
	if want := "migrate-people: document " + notes + " could not be read"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	for _, login := range []string{"ada-example", "bob-example"} {
		if holders := loginHolders(t, database, login); len(holders) != 0 {
			t.Errorf("tables still holding %s after the database moved: %v\n%s", login, holders, out)
		}
	}
	if got, want := documentText(t, database, spec), strings.Replace(specMarkdown, "Ada-Example", ada, 1); got != want {
		t.Errorf("the spec, readable beside the notes, renders %q, want %q", got, want)
	}

	if _, err := database.Pool.Exec(ctx, `
		delete from doc_updates where artifact_id = $1 and version = (select max(version) from doc_updates where artifact_id = $1)
	`, notes); err != nil {
		t.Fatalf("drop the notes' bad update: %v", err)
	}
	out, err = migrate(t, database, examplePeople)
	if err != nil {
		t.Fatalf("the second run, with the notes readable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "migrate-people: before documents.answered_by=1\n") ||
		!strings.Contains(out, "migrate-people: after documents.answered_by=0\n") {
		t.Errorf("the second run did not rename the notes' one answered ask:\n%s", out)
	}
	if got, want := documentText(t, database, notes), strings.Replace(notesMarkdown, "Bob-Example", bob, 1); got != want {
		t.Errorf("the notes render %q, want %q", got, want)
	}
}

// documentText renders artifactID's live state as a reader of the migrated database sees it.
func documentText(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	reader := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer func() { _ = reader.Shutdown(context.Background()) }()
	markdown, err := reader.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("render %s: %v", artifactID, err)
	}
	return markdown
}

func TestParseMap(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		want      Map
		refusal   string
	}{
		{name: "logins in any case, to emails in any case", raw: `{"Ada-Example": "Ada@Example.com", "bob-example": " bob@example.com "}`,
			want: Map{"ada-example": ada, "bob-example": bob}},
		{name: "unset", raw: "", refusal: "DISPATCH_PEOPLE_MAP is required"},
		{name: "not JSON", raw: `ada-example=ada@example.com`, refusal: "not a JSON object"},
		{name: "not an object", raw: `["ada-example"]`, refusal: "not a JSON object"},
		{name: "a value that is not a string", raw: `{"ada-example": 1}`, refusal: "not a JSON object"},
		{name: "an empty map", raw: `{}`, refusal: "names no one"},
		{name: "a key that is already an email", raw: `{"ada@example.com": "ada@example.com"}`, refusal: `"ada@example.com" is not a GitHub login`},
		{name: "an empty key", raw: `{" ": "ada@example.com"}`, refusal: `"" is not a GitHub login`},
		{name: "a value that is not an email", raw: `{"ada-example": "ada"}`, refusal: `"ada-example" maps to "ada", which is not an email`},
		{name: "a value with two at signs", raw: `{"ada-example": "ada@@example.com"}`, refusal: "which is not an email"},
		{name: "one login twice, to two people", raw: `{"Ada-Example": "ada@example.com", "ada-example": "bob@example.com"}`,
			refusal: `"ada-example" is mapped twice`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseMap(test.raw)
			if test.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), test.refusal) {
					t.Fatalf("ParseMap(%q) = %v, %v; want a refusal containing %q", test.raw, got, err, test.refusal)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ParseMap(%q) = %v, %v; want %v", test.raw, got, err, test.want)
			}
		})
	}
}
