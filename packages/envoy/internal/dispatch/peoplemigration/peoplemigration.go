// Package peoplemigration is `envoy-dispatch migrate-people`: it moves every record that names a
// person by the GitHub login Dispatch's GitHub sign-in knew them by to the email its Google
// Workspace sign-in names them by (AGENTC-1563).
//
// A person is named in three shapes. Text columns hold the bare login, in the casing its writer
// stored: the per-person tables, the assignee and a personal token's owner. JSON columns hold the
// actor Dispatch records a write under: a person is {"kind":"user","id":<login>}, and an agent
// writing under a person's token is {"kind":"session","owner":<login>}; the same JSON names a
// person as an issue's "assignee", as an answer's "user", and, at the root of the per-person
// bookkeeping events, as "login". And a document's live state names the person who answered each
// ask in the block's answered_by attribute.
//
// Every value without an "@" in those places is a login; a value with one is already an email
// and is left as it is, which is what makes a second run change nothing. A document's versions
// keep the name they were written with: a version is immutable.
package peoplemigration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Map is DISPATCH_PEOPLE_MAP: each GitHub login Dispatch knew a person by, lowercased, to that
// person's lowercased email.
type Map map[string]string

// ParseMap reads DISPATCH_PEOPLE_MAP, a JSON object from each GitHub login to its person's email.
// GitHub logins are case-insensitive, so a login is matched in any case, and an email is
// lowercased as Dispatch names people. It refuses a login spelled twice to two people.
func ParseMap(raw string) (Map, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("DISPATCH_PEOPLE_MAP is required: a JSON object from each GitHub login to its person's email")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	notObject := func(err error) error {
		return fmt.Errorf("DISPATCH_PEOPLE_MAP is not a JSON object of GitHub logins to emails: %w", err)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		if err == nil {
			err = fmt.Errorf("it opens with %v", token)
		}
		return nil, notObject(err)
	}
	people := Map{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, notObject(err)
		}
		key, _ := token.(string)
		var value string
		if err := decoder.Decode(&value); err != nil {
			return nil, notObject(err)
		}
		login := strings.ToLower(strings.TrimSpace(key))
		if login == "" || strings.Contains(login, "@") {
			return nil, fmt.Errorf("DISPATCH_PEOPLE_MAP: %q is not a GitHub login", login)
		}
		email := strings.ToLower(strings.TrimSpace(value))
		if !isEmail(email) {
			return nil, fmt.Errorf("DISPATCH_PEOPLE_MAP: %q maps to %q, which is not an email", login, value)
		}
		if earlier, mapped := people[login]; mapped && earlier != email {
			return nil, fmt.Errorf("DISPATCH_PEOPLE_MAP: %q is mapped twice, to %q and %q", login, earlier, email)
		}
		people[login] = email
	}
	if _, err := decoder.Token(); err != nil {
		return nil, notObject(err)
	}
	if decoder.More() {
		return nil, notObject(errors.New("it holds more than one value"))
	}
	if len(people) == 0 {
		return nil, errors.New("DISPATCH_PEOPLE_MAP names no one")
	}
	return people, nil
}

func isEmail(value string) bool {
	local, domain, found := strings.Cut(value, "@")
	return found && local != "" && domain != "" && !strings.Contains(domain, "@") &&
		!strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' })
}

// isLogin reports whether value, found where a person is named, names them by login.
func isLogin(value string) bool {
	return value != "" && !strings.Contains(value, "@")
}

// rename is the email people gives login, matched in any case, or login itself when people gives
// it none or it is no login.
func (people Map) rename(login string) string {
	if !isLogin(login) {
		return login
	}
	if email, mapped := people[strings.ToLower(login)]; mapped {
		return email
	}
	return login
}

// loginColumn is a text column holding a person's bare login.
type loginColumn struct {
	table, column string
	// rewrite moves the column's logins to emails through the people_map temporary table. A
	// column that is part of its row's key merges the rows two casings of one login, or a login
	// and the email already there, kept apart: each statement's own comment says how.
	rewrite []string
	// unmapped columns are rewritten without the map: their login rows are deleted.
	unmapped bool
}

var loginColumns = []loginColumn{
	{table: "issues", column: "assignee", rewrite: []string{
		`update issues i set assignee = m.email from people_map m where m.login = lower(i.assignee)`,
	}},
	{table: "agent_tokens", column: "owner", rewrite: []string{
		`update agent_tokens t set owner = m.email from people_map m where m.login = lower(t.owner)`,
	}},
	// One person's state on one issue: pinned if either row was, read as far as the further, every
	// dismissal either row made, and the later change. A row with nothing to merge keeps its
	// dismissals as they were, in their order.
	{table: "user_issue_state", column: "login", rewrite: []string{`
		with moved as (
			select m.email, s.issue_key, s.pinned, s.last_read_seq, s.dismissed, s.seq
			from user_issue_state s join people_map m on m.login = lower(s.login)
		)
		insert into user_issue_state (login, issue_key, pinned, last_read_seq, dismissed, seq)
		select email, issue_key, bool_or(pinned), max(last_read_seq),
			case when count(*) = 1 then min(dismissed::text)::jsonb else (
				select coalesce(jsonb_agg(distinct item), '[]'::jsonb)
				from moved other, jsonb_array_elements(other.dismissed) item
				where other.email = moved.email and other.issue_key = moved.issue_key
			) end,
			max(seq)
		from moved group by email, issue_key
		on conflict (login, issue_key) do update set
			pinned = user_issue_state.pinned or excluded.pinned,
			last_read_seq = greatest(user_issue_state.last_read_seq, excluded.last_read_seq),
			dismissed = (
				select coalesce(jsonb_agg(distinct item), '[]'::jsonb)
				from jsonb_array_elements(user_issue_state.dismissed || excluded.dismissed) item
			),
			seq = greatest(user_issue_state.seq, excluded.seq)
	`, `delete from user_issue_state s using people_map m where m.login = lower(s.login)`}},
	// One person's Clear of one session's conversation: the later one.
	{table: "user_agent_state", column: "login", rewrite: []string{`
		insert into user_agent_state (login, session_id, cleared_before)
		select m.email, s.session_id, max(s.cleared_before)
		from user_agent_state s join people_map m on m.login = lower(s.login)
		group by m.email, s.session_id
		on conflict (login, session_id) do update
		set cleared_before = greatest(user_agent_state.cleared_before, excluded.cleared_before)
	`, `delete from user_agent_state s using people_map m where m.login = lower(s.login)`}},
	// One person's read mark on one session's conversation: the further one.
	{table: "user_agent_read", column: "login", rewrite: []string{`
		insert into user_agent_read (login, session_id, read_through)
		select m.email, r.session_id, max(r.read_through)
		from user_agent_read r join people_map m on m.login = lower(r.login)
		group by m.email, r.session_id
		on conflict (login, session_id) do update
		set read_through = greatest(user_agent_read.read_through, excluded.read_through)
	`, `delete from user_agent_read r using people_map m where m.login = lower(r.login)`}},
	// One person's snooze of one ask: the later wake.
	{table: "user_ask_snooze", column: "login", rewrite: []string{`
		insert into user_ask_snooze (login, ask_id, snoozed_until)
		select m.email, s.ask_id, max(s.snoozed_until)
		from user_ask_snooze s join people_map m on m.login = lower(s.login)
		group by m.email, s.ask_id
		on conflict (login, ask_id) do update
		set snoozed_until = greatest(user_ask_snooze.snoozed_until, excluded.snoozed_until)
	`, `delete from user_ask_snooze s using people_map m where m.login = lower(s.login)`}},
	// One person's idempotency key: the broadcast it already answers keeps it.
	{table: "broadcast_idempotency_keys", column: "login", rewrite: []string{`
		insert into broadcast_idempotency_keys (login, idempotency_key, broadcast_id, request_digest)
		select distinct on (m.email, k.idempotency_key) m.email, k.idempotency_key, k.broadcast_id, k.request_digest
		from broadcast_idempotency_keys k join people_map m on m.login = lower(k.login)
		order by m.email, k.idempotency_key, k.login
		on conflict (login, idempotency_key) do nothing
	`, `delete from broadcast_idempotency_keys k using people_map m where m.login = lower(k.login)`}},
	// A login's session generation can revoke only a cookie naming a login, and no such cookie
	// verifies since cookies name an email (auth.VerifySession), so its row is deleted rather than
	// moved: carried onto the email, a larger generation would revoke the cookie its person has
	// signed in with since the deploy.
	{table: "user_sessions", column: "login", unmapped: true, rewrite: []string{
		`delete from user_sessions where login <> '' and position('@' in login) = 0`,
	}},
}

// actorTable is a table whose JSON columns name people.
type actorTable struct {
	table string
	// key is the table's primary key, which the scan pages through and each update matches.
	key     []keyColumn
	columns []actorColumn
	// typeColumn names the column an actorColumn's root rule reads, for events.
	typeColumn string
}

type keyColumn struct{ name, cast string }

type actorColumn struct {
	name string
	// root is the key at the column value's root that names a person, when one does; rootTypes,
	// when set, limits it to rows whose typeColumn holds one of them.
	root      string
	rootTypes []string
}

// rootPerson is the key at the column value's root that names a person in a row of rowType; ""
// when none does.
func (c actorColumn) rootPerson(rowType string) string {
	if c.rootTypes != nil && !slices.Contains(c.rootTypes, rowType) {
		return ""
	}
	return c.root
}

// perPersonEvents are the event types whose payload names the person at its root, as "login".
var perPersonEvents = []string{"user_state.updated", "user_agent_state.updated"}

var uuidKey = []keyColumn{{"id", "uuid"}}

var actorTables = []actorTable{
	{table: "issues", key: []keyColumn{{"key", "text"}}, columns: []actorColumn{{name: "created_by"}, {name: "claimed_by"}}},
	{table: "artifacts", key: uuidKey, columns: []actorColumn{{name: "created_by"}}},
	{table: "asks", key: uuidKey, columns: []actorColumn{{name: "author"}, {name: "answer", root: "user"}, {name: "resolution"}}},
	{table: "comments", key: uuidKey, columns: []actorColumn{{name: "author"}, {name: "resolved_by"}}},
	{table: "messages", key: uuidKey, columns: []actorColumn{{name: "author"}}},
	{table: "broadcasts", key: uuidKey, columns: []actorColumn{{name: "author"}}},
	{table: "message_deliveries", key: []keyColumn{{"message_id", "uuid"}, {"attempt", "integer"}}, columns: []actorColumn{{name: "requested_by"}}},
	{table: "artifact_reviews", key: uuidKey, columns: []actorColumn{{name: "actor"}}},
	{table: "repo_projects", key: []keyColumn{{"repo", "text"}}, columns: []actorColumn{{name: "created_by"}}},
	{table: "architecture_sources", key: []keyColumn{{"project_key", "text"}}, columns: []actorColumn{{name: "created_by"}}},
	{table: "events", key: []keyColumn{{"id", "bigint"}}, typeColumn: "type",
		columns: []actorColumn{{name: "actor"}, {name: "payload", root: "login", rootTypes: perPersonEvents}}},
}

const documentsField = "documents.answered_by"

// census is what a scan found: for each field, how many rows (ask blocks, for documents) name a
// person by login, and each login it found with the fields holding it.
type census struct {
	counts map[string]int
	logins map[string]map[string]bool
	// mapped are the logins a field needing an email holds, which the map must name.
	mapped map[string]bool
}

func newCensus() *census {
	return &census{counts: map[string]int{}, logins: map[string]map[string]bool{}, mapped: map[string]bool{}}
}

func (c *census) found(field, login string, needsEmail bool) {
	login = strings.ToLower(login)
	if c.logins[login] == nil {
		c.logins[login] = map[string]bool{}
	}
	c.logins[login][field] = true
	if needsEmail {
		c.mapped[login] = true
	}
}

// fields is every field the census reports, in report order.
func fields() []string {
	var names []string
	for _, table := range actorTables {
		for _, column := range table.columns {
			names = append(names, table.table+"."+column.name)
		}
	}
	for _, column := range loginColumns {
		names = append(names, column.table+"."+column.column)
	}
	return append(names, documentsField)
}

func (c *census) write(out io.Writer, label string) {
	for _, field := range fields() {
		fmt.Fprintf(out, "migrate-people: %s %s=%d\n", label, field, c.counts[field])
	}
}

func (c *census) total() int {
	total := 0
	for _, count := range c.counts {
		total += count
	}
	return total
}

// unmapped describes each login the map has no email for, with the fields holding it, sorted.
func (c *census) unmapped(people Map) []string {
	var missing []string
	for login := range c.mapped {
		if _, ok := people[login]; ok {
			continue
		}
		var held []string
		for _, field := range fields() {
			if c.logins[login][field] {
				held = append(held, field)
			}
		}
		missing = append(missing, fmt.Sprintf("%s (%s)", login, strings.Join(held, ", ")))
	}
	sort.Strings(missing)
	return missing
}

func (c *census) addDocuments(answerers []docs.AskAnswerer) {
	for _, answerer := range answerers {
		if isLogin(answerer.AnsweredBy) {
			c.counts[documentsField]++
			c.found(documentsField, answerer.AnsweredBy, true)
		}
	}
}

// Run moves every person documents and database name by GitHub login to the email people gives
// them, writing what it found and what it left to out. It changes nothing when any login it finds
// is missing from people, naming each one and where it was found, since a guessed person would be
// wrong. The database's rows move in one transaction, which commits only once a second scan
// inside it finds no login left; each document's answered asks are then renamed by an update
// appended to its state. Every person a moved record names is recorded in people, so they are an
// assignee the picker offers before they first sign in. Dispatch must not be serving the database
// while it runs: a running server's rooms would not see the documents' appended updates.
func Run(ctx context.Context, database *store.Store, documents *docs.Service, people Map, out io.Writer) error {
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := loadPeopleMap(ctx, tx, people); err != nil {
		return err
	}

	before, err := scanDatabase(ctx, tx)
	if err != nil {
		return err
	}
	answerers, unreadable, err := documents.AskAnswerers(ctx)
	if err != nil {
		return err
	}
	before.addDocuments(answerers)
	before.write(out, "before")
	for _, document := range unreadable {
		fmt.Fprintf(out, "migrate-people: document %s could not be read: %v\n", document.ArtifactID, document.Err)
	}
	if missing := before.unmapped(people); len(missing) > 0 {
		return fmt.Errorf("DISPATCH_PEOPLE_MAP has no email for %s; nothing was changed", strings.Join(missing, "; "))
	}

	for _, column := range loginColumns {
		for _, statement := range column.rewrite {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("move %s.%s: %w", column.table, column.column, err)
			}
		}
	}
	if err := moveActorRows(ctx, tx, people); err != nil {
		return err
	}
	var emails []string
	for login := range before.mapped {
		emails = append(emails, people[login])
	}
	recorded, err := tx.Exec(ctx, `insert into people (email) select unnest($1::text[]) on conflict (email) do nothing`, emails)
	if err != nil {
		return fmt.Errorf("record people: %w", err)
	}
	after, err := scanDatabase(ctx, tx)
	if err != nil {
		return err
	}
	if left := after.total(); left != 0 {
		after.write(out, "after")
		return fmt.Errorf("%d rows still name a person by login after the move; nothing was changed", left)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	var renaming []string
	for _, answerer := range answerers {
		if people.rename(answerer.AnsweredBy) != answerer.AnsweredBy && !slices.Contains(renaming, answerer.ArtifactID) {
			renaming = append(renaming, answerer.ArtifactID)
		}
	}
	var failed []string
	for _, document := range unreadable {
		failed = append(failed, document.ArtifactID)
	}
	for _, renamed := range documents.RenameAskAnswerers(ctx, renaming, people.rename) {
		switch {
		case renamed.Err != nil:
			fmt.Fprintf(out, "migrate-people: document %s not renamed: %v\n", renamed.ArtifactID, renamed.Err)
			failed = append(failed, renamed.ArtifactID)
		case renamed.Skipped != "":
			fmt.Fprintf(out, "migrate-people: document %s not renamed: %s\n", renamed.ArtifactID, renamed.Skipped)
			failed = append(failed, renamed.ArtifactID)
		}
	}
	answerers, _, err = documents.AskAnswerers(ctx)
	if err != nil {
		return err
	}
	after.addDocuments(answerers)
	after.write(out, "after")
	fmt.Fprintf(out, "migrate-people: people recorded=%d\n", recorded.RowsAffected())
	var left []string
	if len(failed) > 0 {
		left = append(left, "these documents could not be read or renamed: "+strings.Join(failed, ", "))
	}
	if asks := after.counts[documentsField]; asks > 0 {
		left = append(left, fmt.Sprintf("%d answered asks still name a login", asks))
	}
	if len(left) > 0 {
		return fmt.Errorf("the database moved, but %s; run it again once they can be", strings.Join(left, "; "))
	}
	return nil
}

// loadPeopleMap writes people into a temporary table the login columns' rewrites join on.
func loadPeopleMap(ctx context.Context, tx pgx.Tx, people Map) error {
	if _, err := tx.Exec(ctx, `create temporary table people_map (login text primary key, email text not null) on commit drop`); err != nil {
		return fmt.Errorf("create the people map: %w", err)
	}
	logins := make([]string, 0, len(people))
	emails := make([]string, 0, len(people))
	for login, email := range people {
		logins = append(logins, login)
		emails = append(emails, email)
	}
	if _, err := tx.Exec(ctx, `insert into people_map (login, email) select * from unnest($1::text[], $2::text[])`, logins, emails); err != nil {
		return fmt.Errorf("load the people map: %w", err)
	}
	return nil
}

// scanDatabase counts the rows of every field that name a person by login, with one query per
// field over its whole table. No count reads a row through pageActorTable, so the census that
// decides whether the run commits cannot share a fault with the read that moves the rows.
func scanDatabase(ctx context.Context, tx pgx.Tx) (*census, error) {
	found := newCensus()
	for _, column := range loginColumns {
		field := column.table + "." + column.column
		rows, err := tx.Query(ctx, fmt.Sprintf(
			`select %[2]s, count(*) from %[1]s where %[2]s <> '' and position('@' in %[2]s) = 0 group by 1`,
			column.table, column.column))
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", field, err)
		}
		for rows.Next() {
			var login string
			var count int
			if err := rows.Scan(&login, &count); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s: %w", field, err)
			}
			found.counts[field] += count
			found.found(field, login, !column.unmapped)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("scan %s: %w", field, err)
		}
	}
	for _, table := range actorTables {
		for _, column := range table.columns {
			if err := countActorColumn(ctx, tx, table, column, found); err != nil {
				return nil, err
			}
		}
	}
	return found, nil
}

// countActorColumn counts the rows whose value in column names a person by login, and each login
// it names, reading the value as swapPeople does: a user actor's id, a session actor's owner, an
// assignee and an answer's user anywhere in it, and the person its root names.
func countActorColumn(ctx context.Context, tx pgx.Tx, table actorTable, column actorColumn, found *census) error {
	field := table.table + "." + column.name
	var root string
	var args []any
	if column.root != "" {
		root = fmt.Sprintf(`
			union all select t.ctid, t.%[2]s->>'%[3]s' from %[1]s t where jsonb_typeof(t.%[2]s->'%[3]s') = 'string'`,
			table.table, column.name, column.root)
		if column.rootTypes != nil {
			root += fmt.Sprintf(` and t.%s = any($1::text[])`, table.typeColumn)
			args = append(args, column.rootTypes)
		}
	}
	rows, err := tx.Query(ctx, fmt.Sprintf(`
		with recursive walk(row, node) as (
			select t.ctid, t.%[2]s from %[1]s t where t.%[2]s is not null
			union all
			select walk.row, child.value from walk, lateral (
				select value from jsonb_each(case when jsonb_typeof(walk.node) = 'object' then walk.node else '{}' end)
				union all
				select value from jsonb_array_elements(case when jsonb_typeof(walk.node) = 'array' then walk.node else '[]' end)
			) child
		), person(row, name) as (
			select row, node->>'id' from walk where node->>'kind' = 'user' and jsonb_typeof(node->'id') = 'string'
			union all select row, node->>'owner' from walk where node->>'kind' = 'session' and jsonb_typeof(node->'owner') = 'string'
			union all select row, node->>'assignee' from walk where jsonb_typeof(node->'assignee') = 'string'
			union all select row, node->'answer'->>'user' from walk where jsonb_typeof(node->'answer'->'user') = 'string'%[3]s
		)
		select lower(name), count(distinct row) from person
		where name <> '' and position('@' in name) = 0
		group by grouping sets ((lower(name)), ())
	`, table.table, column.name, root), args...)
	if err != nil {
		return fmt.Errorf("scan %s: %w", field, err)
	}
	defer rows.Close()
	for rows.Next() {
		// The row without a login is the grouping set over every login: the rows holding any.
		var login *string
		var count int
		if err := rows.Scan(&login, &count); err != nil {
			return fmt.Errorf("scan %s: %w", field, err)
		}
		if login == nil {
			found.counts[field] = count
			continue
		}
		found.found(field, *login, true)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", field, err)
	}
	return nil
}

// moveActorRows rewrites every login in every JSON column to the email people gives it.
func moveActorRows(ctx context.Context, tx pgx.Tx, people Map) error {
	for _, table := range actorTables {
		err := pageActorTable(ctx, tx, table, func(row actorRow) error {
			var sets []string
			var args []any
			for index, column := range table.columns {
				if row.values[index] == nil || !swapPeople(row.values[index], row.rowType, column, people.rename) {
					continue
				}
				encoded, err := encodeJSON(row.values[index])
				if err != nil {
					return fmt.Errorf("encode %s.%s: %w", table.table, column.name, err)
				}
				args = append(args, encoded)
				sets = append(sets, fmt.Sprintf("%s = $%d::jsonb", column.name, len(args)))
			}
			if len(sets) == 0 {
				return nil
			}
			var matches []string
			for index, key := range table.key {
				args = append(args, row.key[index])
				matches = append(matches, fmt.Sprintf("%s = $%d::%s", key.name, len(args), key.cast))
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf("update %s set %s where %s",
				table.table, strings.Join(sets, ", "), strings.Join(matches, " and ")), args...); err != nil {
				return fmt.Errorf("move %s: %w", table.table, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type actorRow struct {
	key     []string
	values  []any
	rowType string
}

// actorPage is how many rows pageActorTable reads at a time: events can be the largest table.
const actorPage = 500

// pageActorTable hands visit every row of table, decoded, in primary-key order, a page at a time;
// visit may write the row, since a page is read whole before any row of it is visited. The key is
// read back as text under a name of its own, and both the order and the cursor name the key column
// itself, qualified so neither can resolve to that text: ordered by the text, a numeric key sorts
// "10000" before "9" while the cursor compares numbers, and every row between is skipped.
func pageActorTable(ctx context.Context, tx pgx.Tx, table actorTable, visit func(actorRow) error) error {
	var keyColumns, keyText, keyArgs []string
	for index, key := range table.key {
		column := table.table + "." + key.name
		keyColumns = append(keyColumns, column)
		keyText = append(keyText, fmt.Sprintf("%s::text as page_key_%d", column, index))
		keyArgs = append(keyArgs, fmt.Sprintf("$%d::%s", index+1, key.cast))
	}
	selected := append(slices.Clone(keyText), func() []string {
		names := make([]string, len(table.columns))
		for index, column := range table.columns {
			names[index] = column.name
		}
		return names
	}()...)
	if table.typeColumn != "" {
		selected = append(selected, table.typeColumn)
	}
	query := "select " + strings.Join(selected, ", ") + " from " + table.table
	order := " order by " + strings.Join(keyColumns, ", ") + fmt.Sprintf(" limit %d", actorPage)
	var after []any
	for {
		statement := query + order
		if after != nil {
			statement = query + " where (" + strings.Join(keyColumns, ", ") + ") > (" + strings.Join(keyArgs, ", ") + ")" + order
		}
		page, err := readActorPage(ctx, tx, table, statement, after)
		if err != nil {
			return err
		}
		for _, row := range page {
			if err := visit(row); err != nil {
				return err
			}
		}
		if len(page) < actorPage {
			return nil
		}
		last := page[len(page)-1].key
		after = make([]any, len(last))
		for index, key := range last {
			after[index] = key
		}
	}
}

func readActorPage(ctx context.Context, tx pgx.Tx, table actorTable, statement string, args []any) ([]actorRow, error) {
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", table.table, err)
	}
	defer rows.Close()
	var page []actorRow
	for rows.Next() {
		keys := make([]string, len(table.key))
		raw := make([][]byte, len(table.columns))
		var rowType string
		targets := make([]any, 0, len(keys)+len(raw)+1)
		for index := range keys {
			targets = append(targets, &keys[index])
		}
		for index := range raw {
			targets = append(targets, &raw[index])
		}
		if table.typeColumn != "" {
			targets = append(targets, &rowType)
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan %s: %w", table.table, err)
		}
		row := actorRow{key: keys, values: make([]any, len(raw)), rowType: rowType}
		for index, value := range raw {
			if value == nil {
				continue
			}
			decoded, err := decodeJSON(value)
			if err != nil {
				return nil, fmt.Errorf("decode %s.%s of %v: %w", table.table, table.columns[index].name, keys, err)
			}
			row.values[index] = decoded
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", table.table, err)
	}
	return page, nil
}

// swapPeople hands swap every person value is holding in column, stores what swap returns in its
// place, and reports whether anything changed.
func swapPeople(value any, rowType string, column actorColumn, swap func(string) string) bool {
	changed := false
	if key := column.rootPerson(rowType); key != "" {
		if root, ok := value.(map[string]any); ok {
			changed = swapString(root, key, swap)
		}
	}
	return swapNested(value, swap) || changed
}

// swapNested swaps every person value anywhere in value: a user actor's id, a session actor's
// owner, an assignee, and an answer's user.
func swapNested(value any, swap func(string) string) bool {
	changed := false
	switch node := value.(type) {
	case map[string]any:
		switch kind, _ := node["kind"].(string); kind {
		case "user":
			changed = swapString(node, "id", swap) || changed
		case "session":
			changed = swapString(node, "owner", swap) || changed
		}
		changed = swapString(node, "assignee", swap) || changed
		if answer, ok := node["answer"].(map[string]any); ok {
			changed = swapString(answer, "user", swap) || changed
		}
		for _, child := range node {
			changed = swapNested(child, swap) || changed
		}
	case []any:
		for _, child := range node {
			changed = swapNested(child, swap) || changed
		}
	}
	return changed
}

func swapString(node map[string]any, key string, swap func(string) string) bool {
	value, ok := node[key].(string)
	if !ok {
		return false
	}
	next := swap(value)
	if next == value {
		return false
	}
	node[key] = next
	return true
}

// decodeJSON decodes a JSON column keeping every number as written.
func decodeJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func encodeJSON(value any) (string, error) {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}
