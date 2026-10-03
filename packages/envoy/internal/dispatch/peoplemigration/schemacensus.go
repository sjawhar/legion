package peoplemigration

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/jackc/pgx/v5"
)

// unsearchedTables are the tables the schema census leaves out, each with why.
var unsearchedTables = map[string]string{
	"artifact_versions": "a document version keeps the names it was written with",
}

// schemaColumn is a column the schema census searches, with the shape its values are read in.
type schemaColumn struct {
	table, column string
	// match is the SQL condition that holds for a row whose value holds a login in $1 as a whole
	// value, with %s standing for the column.
	match string
}

const (
	// A scalar text value is a login when the whole value is one.
	scalarMatch = `lower(%s::text) = any($1)`
	// An array holds a login when one of its elements is one.
	arrayMatch = `exists (select 1 from unnest(%s) element where lower(element::text) = any($1))`
	// A JSON value holds a login when a string anywhere in it, or an object's key, is one.
	jsonMatch = `(exists (
		select 1 from jsonb_path_query(%[1]s::jsonb, 'strict $.**') value
		where jsonb_typeof(value) = 'string' and lower(value #>> '{}') = any($1)
	) or exists (
		select 1 from jsonb_path_query(%[1]s::jsonb, 'strict $.** ? (@.type() == "object")') object,
			jsonb_object_keys(object) key
		where lower(key) = any($1)
	))`
)

// searchedColumns lists every column of every table in the database's own schema that can hold a
// person's login: text, character varying, character and citext columns, arrays of those, and
// json and jsonb columns, read from information_schema.columns so a table added after this code
// was written is searched too. It leaves out unsearchedTables.
func searchedColumns(ctx context.Context, tx pgx.Tx) ([]schemaColumn, error) {
	rows, err := tx.Query(ctx, `
		select c.table_name, c.column_name, c.data_type, c.udt_name
		from information_schema.columns c
		join information_schema.tables t on t.table_schema = c.table_schema and t.table_name = c.table_name
		where c.table_schema = current_schema() and t.table_type = 'BASE TABLE'
		  and (c.data_type in ('text', 'character varying', 'character', 'json', 'jsonb')
		    or (c.data_type = 'USER-DEFINED' and c.udt_name = 'citext')
		    or (c.data_type = 'ARRAY' and c.udt_name in ('_text', '_varchar', '_bpchar', '_citext')))
		order by c.table_name, c.ordinal_position
	`)
	if err != nil {
		return nil, fmt.Errorf("list the schema's columns: %w", err)
	}
	defer rows.Close()
	var columns []schemaColumn
	for rows.Next() {
		var table, column, dataType, udtName string
		if err := rows.Scan(&table, &column, &dataType, &udtName); err != nil {
			return nil, fmt.Errorf("list the schema's columns: %w", err)
		}
		if _, skipped := unsearchedTables[table]; skipped {
			continue
		}
		match := scalarMatch
		switch dataType {
		case "ARRAY":
			match = arrayMatch
		case "json", "jsonb":
			match = jsonMatch
		}
		columns = append(columns, schemaColumn{table: table, column: column, match: match})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the schema's columns: %w", err)
	}
	return columns, nil
}

// schemaCensus counts, in every column searchedColumns lists, the rows holding a login people
// names as a whole value: the value itself, an array's element, or a string or object key
// anywhere in a JSON value, each compared lowercased. It writes one line per column, names each
// table it leaves out and why, and returns each column holding any, as "<table>.<column> (<n>
// rows)". Prose is not searched for a login inside it: a comment's body, a title or a document's
// text that mentions a login mentions it, and names no one Dispatch acts for.
func schemaCensus(ctx context.Context, tx pgx.Tx, people Map, out io.Writer) ([]string, error) {
	columns, err := searchedColumns(ctx, tx)
	if err != nil {
		return nil, err
	}
	logins := make([]string, 0, len(people))
	for login := range people {
		logins = append(logins, login)
	}
	skipped := make([]string, 0, len(unsearchedTables))
	for table := range unsearchedTables {
		skipped = append(skipped, table)
	}
	sort.Strings(skipped)
	for _, table := range skipped {
		fmt.Fprintf(out, "migrate-people: census skips %s: %s\n", table, unsearchedTables[table])
	}
	var hits []string
	for _, column := range columns {
		name := pgx.Identifier{column.column}.Sanitize()
		var count int
		if err := tx.QueryRow(ctx, fmt.Sprintf(`select count(*) from %s t where %s`,
			pgx.Identifier{column.table}.Sanitize(), fmt.Sprintf(column.match, "t."+name)), logins,
		).Scan(&count); err != nil {
			return nil, fmt.Errorf("census %s.%s: %w", column.table, column.column, err)
		}
		fmt.Fprintf(out, "migrate-people: census %s.%s=%d\n", column.table, column.column, count)
		if count > 0 {
			hits = append(hits, fmt.Sprintf("%s.%s (%d rows)", column.table, column.column, count))
		}
	}
	fmt.Fprintf(out, "migrate-people: census searched %d columns, %d hold a login\n", len(columns), len(hits))
	return hits, nil
}
