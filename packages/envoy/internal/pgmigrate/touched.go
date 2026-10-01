package pgmigrate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// rowWrite is what a statement does to the rows of the table it names, which decides the tables
// a foreign key reaches from it (CensusTables). A statement that writes no row (DDL, lock table, a
// foreign key's references) is 0.
type rowWrite int

const (
	insertsRows rowWrite = 1 << iota
	updatesRows
	deletesRows
	truncatesRows
)

// touchedTablePatterns are the statement forms the repository's migrations lock a table above
// ACCESS SHARE with, each with what it does to that table's rows.
var touchedTablePatterns = []struct {
	pattern *regexp.Regexp
	writes  rowWrite
}{
	{regexp.MustCompile(`(?is)\balter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?([\w."]+)`), 0},
	{regexp.MustCompile(`(?is)\bcreate\s+(?:unique\s+)?index\s+(?:concurrently\s+)?(?:if\s+not\s+exists\s+)?[\w."]+\s+on\s+(?:only\s+)?([\w."]+)`), 0},
	{regexp.MustCompile(`(?is)\bdrop\s+table\s+(?:if\s+exists\s+)?([\w."]+(?:\s*,\s*[\w."]+)*)`), 0},
	{regexp.MustCompile(`(?is)\btruncate\s+(?:table\s+)?(?:only\s+)?([\w."]+(?:\s*,\s*[\w."]+)*)`), truncatesRows},
	{regexp.MustCompile(`(?is)\bcreate\s+(?:or\s+replace\s+)?(?:constraint\s+)?trigger\s+[\w."]+.*?\bon\s+([\w."]+)`), 0},
	// The table may carry an alias (update comments c set …), as 0043 and 0044 write it.
	{regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?([\w."]+)(?:\s+(?:as\s+)?[\w"]+)?\s+set\b`), updatesRows},
	{regexp.MustCompile(`(?is)\bdelete\s+from\s+(?:only\s+)?([\w."]+)`), deletesRows},
	{regexp.MustCompile(`(?is)\binsert\s+into\s+([\w."]+)`), insertsRows},
	// An upsert's do update can change any column of the row it meets, its key among them. A
	// statement holds no ; outside a literal, and migrationCode empties every literal.
	{regexp.MustCompile(`(?is)\binsert\s+into\s+([\w."]+)[^;]*?\bon\s+conflict\b[^;]*?\bdo\s+update\b`), updatesRows},
	{regexp.MustCompile(`(?is)\block\s+(?:table\s+)?(?:only\s+)?([\w."]+(?:\s*,\s*[\w."]+)*)`), 0},
	// A foreign key takes SHARE ROW EXCLUSIVE on the table it references, which holds that
	// table's writes for as long as the migration runs.
	{referencesPattern, 0},
}

var referencesPattern = regexp.MustCompile(`(?is)\breferences\s+(?:only\s+)?([\w."]+)`)

var touchedIndexPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\bdrop\s+index\s+(?:concurrently\s+)?(?:if\s+exists\s+)?([\w."]+)`),
	regexp.MustCompile(`(?is)\balter\s+index\s+(?:if\s+exists\s+)?([\w."]+)`),
}

// TouchedTables names, sorted and once each, the tables a migration's statements lock above
// ACCESS SHARE: the targets of alter table, create index … on, drop table, truncate, create
// trigger … on, update (with or without an alias), delete from, insert into and lock table, and
// the table a foreign key references. A table a statement only reads (insert … select from, create
// view … as) is left out: its ACCESS SHARE waits only behind an ACCESS EXCLUSIVE holder. It reads
// the statements the migration runs (migrationCode): not its comments, its string literals or the
// body of a function it defines, and a DO block's body as statements; it lowercases and strips a
// public. qualifier and quotes. The forms are the ones the repository's migrations use. What it
// reads from the text alone is part of the census's reading, CensusTables, which every store's
// tests hold to what each of its migrations really locks
// (pgmigratetest.CheckTouchedTablesAgainstLocks), so a migration written in a form this does not
// know (reindex, cluster, create policy … on, merge into) fails there and needs a pattern here.
func TouchedTables(sql string) []string {
	return touchedNames(readMigration(sql).code)
}

// touchedNames names, sorted and once each, the tables a migration's code names (touches).
func touchedNames(code string) []string {
	var names []string
	for _, touch := range touches(code) {
		names = append(names, touch.name)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// touchedIndexes names the indexes a migration's code (migrationCode) drops or alters.
func touchedIndexes(code string) []string {
	var names []string
	for _, pattern := range touchedIndexPatterns {
		names = append(names, namesMatching(code, pattern)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// touch is one table a statement of a migration names, with what the statement does to its rows.
type touch struct {
	name   string
	writes rowWrite
}

func touches(code string) []touch {
	var out []touch
	for _, form := range touchedTablePatterns {
		for _, name := range namesMatching(code, form.pattern) {
			out = append(out, touch{name: name, writes: form.writes})
		}
	}
	return out
}

func namesMatching(code string, pattern *regexp.Regexp) []string {
	var names []string
	for _, match := range pattern.FindAllStringSubmatch(code, -1) {
		for _, raw := range strings.Split(match[1], ",") {
			name := strings.ToLower(strings.Trim(strings.TrimSpace(raw), `"`))
			name = strings.TrimPrefix(name, "public.")
			if name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// Querier is what CensusTables reads the catalog through: the census's transaction (pgx.Tx), or
// the connection the lock audit applies a set on (*pgx.Conn).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TouchedTable is one table of the census's reading of a migration (CensusTables).
type TouchedTable struct {
	Name string
	// Through is the table a foreign key reaches this one from, when no statement of the migration
	// names it; empty for a table one names.
	Through string
	// Conditional says only statements inside a DO block's body name the table, directly or
	// through an index: the block can branch on what the database holds, so whether the migration
	// locks the table can depend on its rows. The census reads it all the same.
	Conditional bool
}

// CensusTables is the census's one reading of the tables a migration locks above ACCESS SHARE,
// sorted by name: Census checks each, and pgmigratetest.CheckTouchedTablesAgainstLocks holds the
// reading to the locks each migration of a set takes. It reads, through q's catalog:
//
//   - every table TouchedTables names;
//   - the table of each index the migration drops or alters (an index the database does not have
//     yet is skipped: the pending migration that creates it names its table);
//   - every table a foreign key reaches from rows the migration writes, which a statement locks
//     only when it writes a row. A row inserted or updated is checked against the table its foreign
//     key references (ROW SHARE). A row deleted, or one whose key an update changes, is checked
//     against every table that references it (ROW SHARE) or acted on there by the key's on delete
//     or on update (ROW EXCLUSIVE); a cascaded delete goes on from that table, as truncate …
//     cascade goes on from every table it reaches, while a set null or a cascaded update stops at
//     the table it writes. An insert's on conflict do update is read as an update too.
//
// The foreign keys followed are the ones the catalog holds, read when the census is taken; Census
// adds the ones earlier pending migrations add (addedForeignKeys), which the lock audit, applying
// each migration before it reads the next, finds in the catalog.
func CensusTables(ctx context.Context, q Querier, sql string) ([]TouchedTable, error) {
	keys, err := foreignKeys(ctx, q)
	if err != nil {
		return nil, err
	}
	return censusTables(ctx, q, readMigration(sql), keys)
}

// censusTables is CensusTables over a migration as the census reads it, following keys, the
// foreign keys the database holds by the time the migration applies.
func censusTables(ctx context.Context, q Querier, text migrationText, keys []foreignKey) ([]TouchedTable, error) {
	writes := map[string]rowWrite{}
	for _, touch := range touches(text.code) {
		writes[touch.name] |= touch.writes
	}
	unconditional := map[string]bool{}
	for _, name := range touchedNames(text.outsideBlocks) {
		unconditional[name] = true
	}
	indexesOutsideBlocks := touchedIndexes(text.outsideBlocks)
	for _, index := range touchedIndexes(text.code) {
		var table string
		err := q.QueryRow(ctx, "select indrelid::regclass::text from pg_index where indexrelid = to_regclass($1)", index).Scan(&table)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("resolve index %s: %w", index, err)
		}
		table = strings.TrimPrefix(table, "public.")
		if _, named := writes[table]; !named {
			writes[table] = 0
		}
		if slices.Contains(indexesOutsideBlocks, index) {
			unconditional[table] = true
		}
	}
	tables := make([]TouchedTable, 0, len(writes))
	for name := range writes {
		tables = append(tables, TouchedTable{Name: name, Conditional: !unconditional[name]})
	}
	for name, through := range reachedByForeignKeys(writes, keys) {
		if _, named := writes[name]; !named {
			tables = append(tables, TouchedTable{Name: name, Through: through})
		}
	}
	slices.SortFunc(tables, func(a, b TouchedTable) int { return strings.Compare(a.Name, b.Name) })
	return tables, nil
}

// foreignKey is one foreign key of the database: the table that holds it, the table it
// references, and its on delete action (pg_constraint.confdeltype: c is cascade).
type foreignKey struct {
	from, to, onDelete string
}

func foreignKeys(ctx context.Context, q Querier) ([]foreignKey, error) {
	rows, err := q.Query(ctx, `
		select conrelid::regclass::text, confrelid::regclass::text, confdeltype::text
		from pg_constraint where contype = 'f'
		order by 1, 2, conname`)
	if err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (foreignKey, error) {
		var key foreignKey
		err := row.Scan(&key.from, &key.to, &key.onDelete)
		key.from, key.to = strings.TrimPrefix(key.from, "public."), strings.TrimPrefix(key.to, "public.")
		return key, err
	})
	if err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	return keys, nil
}

// statementTable is the table a create table or alter table statement is about.
var statementTable = regexp.MustCompile(`(?is)\b(?:create\s+(?:unlogged\s+)?table\s+(?:if\s+not\s+exists\s+)?|alter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?)([\w."]+)`)

var onDeleteCascade = regexp.MustCompile(`(?is)\bon\s+delete\s+cascade\b`)

// addedForeignKeys reads, from a migration's code, the foreign keys it adds: each references in a
// create table or alter table statement, from that statement's table. The statement is found
// anywhere in the text between two semicolons, as the lock patterns find theirs, so one inside a DO
// block's body counts, behind an if not exists over pg_constraint or not. A statement adding one
// with on delete cascade has every key it adds read as cascading, which reaches more tables, never
// fewer.
func addedForeignKeys(code string) []foreignKey {
	var keys []foreignKey
	for _, segment := range strings.Split(code, ";") {
		start := statementTable.FindStringIndex(segment)
		if start == nil {
			continue
		}
		statement := segment[start[0]:]
		from := namesMatching(statement, statementTable)
		if len(from) == 0 {
			continue
		}
		onDelete := "a"
		if onDeleteCascade.MatchString(statement) {
			onDelete = "c"
		}
		for _, to := range namesMatching(statement, referencesPattern) {
			keys = append(keys, foreignKey{from: from[0], to: to, onDelete: onDelete})
		}
	}
	return keys
}

// reachedByForeignKeys names each table a foreign key reaches from the rows writes says the
// migration writes, with the table it is reached from (CensusTables says which keys reach where).
func reachedByForeignKeys(writes map[string]rowWrite, keys []foreignKey) map[string]string {
	reached := map[string]string{}
	reach := func(table, from string) {
		if _, ok := reached[table]; !ok {
			reached[table] = from
		}
	}
	type removal struct {
		table     string
		truncates bool
	}
	var removals []removal
	for _, table := range slices.Sorted(maps.Keys(writes)) {
		w := writes[table]
		for _, key := range keys {
			if w&(insertsRows|updatesRows) != 0 && key.from == table {
				reach(key.to, table)
			}
			if w&updatesRows != 0 && key.to == table {
				reach(key.from, table)
			}
		}
		if w&deletesRows != 0 {
			removals = append(removals, removal{table: table})
		}
		if w&truncatesRows != 0 {
			removals = append(removals, removal{table: table, truncates: true})
		}
	}
	done := map[removal]bool{}
	for len(removals) > 0 {
		r := removals[0]
		removals = removals[1:]
		if done[r] {
			continue
		}
		done[r] = true
		for _, key := range keys {
			if key.to != r.table {
				continue
			}
			reach(key.from, r.table)
			if r.truncates || key.onDelete == "c" {
				removals = append(removals, removal{table: key.from, truncates: r.truncates})
			}
		}
	}
	return reached
}

// migrationText is a migration's SQL as the census reads it.
type migrationText struct {
	// code is the text of the statements the migration runs (migrationCode), a DO block's body
	// among them.
	code string
	// outsideBlocks is code with each DO block's body read as a literal, as a function's is: the
	// statements the migration runs whatever rows its database holds.
	outsideBlocks string
}

func readMigration(sql string) migrationText {
	return migrationText{code: migrationCode(sql, true), outsideBlocks: migrationCode(sql, false)}
}

// migrationCode is the text of the statements a migration runs, for the patterns to read, found
// with scan.l's rules (scanSQL), so a -- or /* inside a literal is the literal's. Each comment is a
// space and each string literal is an empty one. A dollar-quoted literal is read as statements only
// as a DO statement's body, which the migration runs, and only when blocks says so; anywhere else
// it is an empty literal, a function's body among them, which the migration defines and does not
// run.
func migrationCode(sql string, blocks bool) string {
	var code strings.Builder
	statement := "" // the first word of the statement being read
	scanSQL(sql, func(kind sqlTokenKind, start, end int) error {
		text := sql[start:end]
		switch {
		case kind == sqlComment:
			code.WriteByte(' ')
		case kind == sqlString, kind == sqlUnicodeEscape && text[2] == '\'':
			code.WriteString("''")
		case kind == sqlDollarString && blocks && statement == "do":
			open := strings.IndexByte(text[1:], '$') + 2
			code.WriteByte(' ')
			code.WriteString(migrationCode(strings.TrimSuffix(text[open:], text[:open]), blocks))
			code.WriteByte(' ')
		case kind == sqlDollarString:
			code.WriteString("''")
		default:
			switch {
			case kind == sqlWord && statement == "":
				statement = strings.ToLower(text)
			case text == ";":
				statement = ""
			}
			code.WriteString(text)
		}
		return nil
	})
	return code.String()
}
