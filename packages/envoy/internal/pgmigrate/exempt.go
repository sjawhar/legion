package pgmigrate

import (
	"encoding/json"
	"slices"
	"strings"

	pgparser "github.com/wasilibs/go-pgquery/parser"
)

// catalogOnlyStatements are the statement kinds a migration may be made of and still be exempt
// from CensusTableLimit, keyed by the parse node Postgres's own grammar gives each, with the words
// the report names them by and the check a statement of that kind must pass. The command itself
// does its work in the system catalog and reads no row of a table, so the time it holds its lock
// does not grow with the table:
//
//   - create function (or procedure, with or without or replace), in plpgsql or sql only
//     (functionIsCatalogOnly): creating one writes pg_proc and has the language's validator check
//     the body, which for plpgsql parses it and for sql analyzes its queries, taking at most
//     ACCESS SHARE on the tables they name and reading none of their rows. A function in any other
//     language is not exempt: creating a C function loads its library, which can run any code;
//   - create trigger, of any form: it takes SHARE ROW EXCLUSIVE on its table and writes
//     pg_trigger, and the trigger runs only on later writes, never on the rows already there.
//
// An event trigger on these commands runs code of its own when they run, so Census does not apply
// the exemption where one is enabled (ddlEventTriggers). Adding a kind is a reviewed edit with the
// same argument for it; a kind that can scan or rewrite (create index, alter table, a DO block,
// any DML) never belongs here.
var catalogOnlyStatements = map[string]struct {
	kind  string
	check func(node json.RawMessage) bool
}{
	"CreateFunctionStmt": {"create function", functionIsCatalogOnly},
	"CreateTrigStmt":     {"create trigger", func(json.RawMessage) bool { return true }},
}

// functionIsCatalogOnly reports whether a create function's parse node names plpgsql or sql as its
// language, or names none and has an SQL-standard body (begin atomic, return), which is sql.
func functionIsCatalogOnly(node json.RawMessage) bool {
	var function struct {
		Options []struct {
			DefElem *struct {
				Defname string `json:"defname"`
				Arg     struct {
					String *struct {
						Sval string `json:"sval"`
					} `json:"String"`
				} `json:"arg"`
			} `json:"DefElem"`
		} `json:"options"`
		SQLBody json.RawMessage `json:"sql_body"`
	}
	if err := json.Unmarshal(node, &function); err != nil {
		return false
	}
	language := ""
	for _, option := range function.Options {
		if option.DefElem != nil && option.DefElem.Defname == "language" {
			if option.DefElem.Arg.String == nil {
				return false
			}
			language = strings.ToLower(option.DefElem.Arg.String.Sval)
		}
	}
	switch language {
	case "plpgsql", "sql":
		return true
	case "":
		return len(function.SQLBody) > 0
	}
	return false
}

// tableSizeExemption says why migration sql is exempt from CensusTableLimit: the kinds of its
// statements, as the report names them ("create function and create trigger"), when every one of
// its statements is a catalogOnlyStatements kind and passes that kind's check; "" when any is not
// or does not, when it holds no statement, or when it does not parse.
//
// The statements are read by Postgres's own parser, libpg_query (Postgres 17's scan.l and
// gram.y), run as WebAssembly in pure Go by github.com/wasilibs/go-pgquery, so the reading is the
// server's: a comment, a string literal or a dollar-quoted function body is a token of the
// statement it is in, never a statement, and two statements on one line are two parse nodes. A
// migration that does not parse is not exempt; the server would refuse it anyway. A failure of
// the parser itself is not exempt either, so the size rule stands whenever the reading cannot be
// had.
func tableSizeExemption(sql string) (exemption string) {
	defer func() {
		if recover() != nil {
			exemption = ""
		}
	}()
	tree, err := pgparser.ParseToJSON(sql)
	if err != nil {
		return ""
	}
	var parsed struct {
		Stmts []struct {
			Stmt map[string]json.RawMessage `json:"stmt"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(tree), &parsed); err != nil || len(parsed.Stmts) == 0 {
		return ""
	}
	var kinds []string
	for _, statement := range parsed.Stmts {
		if len(statement.Stmt) != 1 {
			return ""
		}
		for node, body := range statement.Stmt {
			allowed, ok := catalogOnlyStatements[node]
			if !ok || !allowed.check(body) {
				return ""
			}
			if !slices.Contains(kinds, allowed.kind) {
				kinds = append(kinds, allowed.kind)
			}
		}
	}
	return strings.Join(kinds, " and ")
}

// ddlEventTriggersQuery names, sorted and comma-separated, the enabled event triggers that run when
// a create function, create procedure or create trigger runs; "" when there are none.
const ddlEventTriggersQuery = `
	select coalesce(string_agg(evtname, ', ' order by evtname), '')
	from pg_event_trigger
	where evtenabled <> 'D' and evtevent in ('ddl_command_start', 'ddl_command_end')
		and (evttags is null or evttags && array['CREATE FUNCTION', 'CREATE PROCEDURE', 'CREATE TRIGGER'])`
