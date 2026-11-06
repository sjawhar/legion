package pgmigrate

import (
	"encoding/json"
	"slices"
	"strings"

	pgparser "github.com/wasilibs/go-pgquery/parser"
)

// catalogOnlyStatements are the statement kinds a migration may be made of and still be exempt
// from CensusTableLimit, keyed by the parse node Postgres's own grammar gives each, each node's
// function answering the word the report names the statement by, or "" where a statement of that
// node is not one of these kinds. The command itself does its work in the system catalog and
// reads no row of a table, so the time it holds its lock does not grow with the table:
//
//   - create function (or procedure, with or without or replace), in plpgsql or sql only
//     (catalogOnlyFunction): creating one writes pg_proc and has the language's validator check
//     the body, which for plpgsql parses it and for sql analyzes its queries, taking at most
//     ACCESS SHARE on the tables they name and reading none of their rows. A function in any other
//     language is not exempt: creating a C function loads its library, which can run any code;
//   - create trigger, of any form: it takes SHARE ROW EXCLUSIVE on its table and writes
//     pg_trigger, and the trigger runs only on later writes, never on the rows already there;
//   - drop trigger and drop function, with RESTRICT, which is the default (catalogOnlyDrop):
//     dropping a trigger writes pg_trigger and dropping a function writes pg_proc, and neither
//     reads a row of the table or removes one, so the time the statement holds its lock does not
//     grow with the table. The lock is not the argument for it: drop trigger takes ACCESS
//     EXCLUSIVE on its table where create trigger takes SHARE ROW EXCLUSIVE, and what the limit
//     is about is how long a lock is held, not which lock it is. A drop with cascade is not
//     exempt: what it drops besides the object it names is not in the statement, so the census
//     cannot read which objects a cascade reaches, and the exemption is an argument about the
//     statement the census can read. drop procedure is left out for the one reason that no
//     migration of this repository drops a procedure;
//   - set and reset (VariableSetStmt), local or session, set role included: a run-time parameter,
//     which takes no lock, reads no row and names no table, so it cannot make a migration's lock
//     window grow with any table, and the other statements are still held to the kinds above.
//     A migration that bounds its own lock wait sets lock_timeout
//     (0066_asks_handed_back_reply_fkey, 20261101182522_search_term_queue_triggers), and that
//     line must not be what costs it an exemption the rest of its statements earn.
//
// An event trigger on these commands runs code of its own when they run, so Census does not apply
// the exemption where one is enabled (ddlEventTriggers). Adding a kind is a reviewed edit with the
// same argument for it; a kind that can scan or rewrite (create index, alter table, a DO block,
// any DML) never belongs here.
var catalogOnlyStatements = map[string]func(node json.RawMessage) string{
	"CreateFunctionStmt": catalogOnlyFunction,
	"CreateTrigStmt":     func(json.RawMessage) string { return "create trigger" },
	"DropStmt":           catalogOnlyDrop,
	"VariableSetStmt":    func(json.RawMessage) string { return "set" },
}

// catalogOnlyFunction names a create function's kind, "create function", when its parse node
// names plpgsql or sql as its language, or names none and has an SQL-standard body (begin
// atomic, return), which is sql; "" for any other language and for a node it cannot read.
func catalogOnlyFunction(node json.RawMessage) string {
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
		return ""
	}
	language := ""
	for _, option := range function.Options {
		if option.DefElem != nil && option.DefElem.Defname == "language" {
			if option.DefElem.Arg.String == nil {
				return ""
			}
			language = strings.ToLower(option.DefElem.Arg.String.Sval)
		}
	}
	switch language {
	case "plpgsql", "sql":
		return "create function"
	case "":
		if len(function.SQLBody) > 0 {
			return "create function"
		}
	}
	return ""
}

// catalogOnlyDrop names a drop's kind as the report names it, "drop trigger" or "drop function",
// and answers "" for a drop of anything else (a table, an index, a view, a schema, a procedure),
// for a drop with cascade, and for a node it cannot read. catalogOnlyStatements argues both.
func catalogOnlyDrop(node json.RawMessage) string {
	var drop struct {
		RemoveType string `json:"removeType"`
		Behavior   string `json:"behavior"`
	}
	if err := json.Unmarshal(node, &drop); err != nil || drop.Behavior != "DROP_RESTRICT" {
		return ""
	}
	switch drop.RemoveType {
	case "OBJECT_TRIGGER":
		return "drop trigger"
	case "OBJECT_FUNCTION":
		return "drop function"
	}
	return ""
}

// tableSizeExemption says why migration sql is exempt from CensusTableLimit: the kinds of its
// statements, as the report names them ("create function and create trigger"), when every one of
// its statements is a catalogOnlyStatements kind; "" when any statement is not one of those
// kinds, when the migration holds no statement, or when it does not parse.
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
			catalogOnly, ok := catalogOnlyStatements[node]
			if !ok {
				return ""
			}
			kind := catalogOnly(body)
			if kind == "" {
				return ""
			}
			if !slices.Contains(kinds, kind) {
				kinds = append(kinds, kind)
			}
		}
	}
	return strings.Join(kinds, " and ")
}

// ddlEventTriggersQuery names, sorted and comma-separated, every enabled event trigger on
// ddl_command_start, ddl_command_end or sql_drop whose tags are unset or name one of the five
// command tags below, the DDL commands catalogOnlyStatements admits (its set and reset fire no
// event trigger, and a create function's command tag is CREATE FUNCTION or CREATE PROCEDURE);
// "" when there are none. A drop fires sql_drop as well as the two ddl_command_ events, so an
// event trigger on sql_drop would run code of its own on a drop the exemption covers. Census
// reads this once for the whole database rather than per migration, so such a trigger on
// sql_drop takes the exemption from every migration, one holding no drop included: the reading
// that refuses, not the one that applies.
const ddlEventTriggersQuery = `
	select coalesce(string_agg(evtname, ', ' order by evtname), '')
	from pg_event_trigger
	where evtenabled <> 'D' and evtevent in ('ddl_command_start', 'ddl_command_end', 'sql_drop')
		and (evttags is null or evttags && array['CREATE FUNCTION', 'CREATE PROCEDURE', 'CREATE TRIGGER', 'DROP FUNCTION', 'DROP TRIGGER'])`
