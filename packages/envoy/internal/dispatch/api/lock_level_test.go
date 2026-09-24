package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ownerLockTables are the tables the room lock's rule covers: the two owner rows of a document,
// the project they belong to, and asks, a foreign-key parent a settlement holds across the room
// lock. See lockDocumentRoom in internal/dispatch/docs/persistence.go.
var ownerLockTables = []string{"issues", "artifacts", "projects", "asks"}

var (
	sqlWhitespace   = regexp.MustCompile(`\s+`)
	forUpdateClause = regexp.MustCompile(`\bfor update\b`)
	ownerLockTable  = regexp.MustCompile(`\b(?:` + strings.Join(ownerLockTables, "|") + `)\b`)
)

// TestNoForUpdateOnOwnerTables enforces the prohibition the room lock's rule states: no statement
// in this module locks issues, artifacts, projects or asks `for update`. It is the prohibition
// only. A weaker level on those tables is legal and deliberate - `for no key update` where a
// writer must serialise against other writers of the same row, `for key share` on an artifact
// whose anchors are being reconciled (api/anchors.go), `for share` on the asks a table edit
// checks (docs/mutation.go) - and none of them blocks the key share a foreign key takes. Only
// `for update` does, and under it a durable writer holding the room lock deadlocks against
// whoever holds the owner row.
//
// Its reach is the statement as this scan can assemble it: a `for update` spelled in a string
// literal, or in a chain of literals and package-level constants, `var`s included, resolved to
// a fixpoint so a constant written from another constant carries the inner text too. That is
// how every owner-table lock in this module is written, sixteen statements in all - eleven a
// plain `select ... for no key update`, three a plain `for key share` or `for share` at the
// weaker levels named above, and two a row shape welded in from constants at the ask reads:
// api/asks.go's `select `+askRowColumns+askRowFrom+` where a.id = $1 for no key update of a`,
// whose askRowColumns is docs.AskColumns plus its own columns, and docs/ask_blocks.go's
// `select `+AskColumns.
//
// An operand the scan cannot resolve - a call, a runtime value - contributes its own string
// literals instead, joined by a space and padded at both ends. A clause those literals spell is
// refused even when no one of them spells it whole: strings.Join([]string{"for", "update"}, " ")
// reds. The padding is a space rather than a boundary, so a clause that forms across the splice
// reds too: "select key from issues for" + strings.Join([]string{"update"}, "") does. A keyword
// split mid-word never forms, so strings.Repeat("for up", 1) + "date" is outside that reach, and
// keeping such a site off `for update` is a review matter rather than a guarantee this check
// makes.
//
// `for update` elsewhere is untouched: doc_checkpoints, comments, messages, message_deliveries,
// comment_mentions, comment_deliveries, user_issue_state and architecture_sources all use it.
//
// The match is statement-scoped and conservative, and it prints the statement rather than naming
// a table, because which row a statement locks is a property of the whole statement. `for update`
// with no `of` locks rows in every table of the FROM, so an owner table reached through a join is
// really locked and that red is a true positive; Postgres refuses `for update` on the nullable
// side of an outer join, so `left join artifacts ... for update` cannot exist in working code.
// Two shapes can red without a real offence, and each has its own tell in the printed statement.
// `for update of <alias>` where the alias names a table outside this rule and an owner table is
// merely joined beside it: the tell is the `of <alias>` the statement carries, which names the
// row the lock really takes. A statement concatenated with an expression this scan cannot
// resolve, whose own string literals are folded in - a literal that never reaches the query can
// put an owner table's name into the scanned text, as `... doc_checkpoints ... for update` +
// strings.Repeat("issues", 0) prints `... for update issues`: the tell is a word the statement
// did not spell, an owner table outside its `from` or a lock clause the query never assembles.
func TestNoForUpdateOnOwnerTables(t *testing.T) {
	sources := moduleSources(t, "../../..")
	constants := stringConstants(t, sources)
	var offences []string
	seen := map[string]struct{}{}
	for _, source := range sources {
		for _, statement := range sqlStatements(source.file, constants) {
			if !locksOwnerTableForUpdate(statement) {
				continue
			}
			offence := filepath.ToSlash(source.path) + ": " + statement
			if _, repeat := seen[offence]; repeat {
				continue
			}
			seen[offence] = struct{}{}
			offences = append(offences, offence)
		}
	}
	if len(offences) > 0 {
		t.Fatalf("`for update` is refused on %s:\n\t%s\n"+
			"Take a weaker level - unless this transaction deletes the row or changes a key "+
			"column, which the weaker levels do not cover: then revisit the rule at lockDocumentRoom.",
			strings.Join(ownerLockTables, ", "), strings.Join(offences, "\n\t"))
	}
}

// locksOwnerTableForUpdate reports whether one normalised statement locks `for update` with an
// in-scope table in it. It matches within the whole statement rather than one line or one
// literal: the SQL is written across several lines of a raw string, and a row shape is often
// concatenated from constants, so the clause and its table routinely sit apart.
func locksOwnerTableForUpdate(statement string) bool {
	return forUpdateClause.MatchString(statement) && ownerLockTable.MatchString(statement)
}

// moduleSource is one non-test Go file of the module with the tree both scans below read. The
// file is parsed once here rather than again in each scan: the constant map and the statement
// walk want the same trees, and parsing the module twice doubled this test's wall time.
type moduleSource struct {
	path string
	file *ast.File
}

// moduleSources reads and parses every non-test Go file of the envoy module, not just the
// Dispatch server: the broker's owner locks live in internal/events, and a future caller could
// lock one of these rows from anywhere. Test sources hold these strings deliberately - the
// upload regressions and the sabotage copies a red proof makes - so they are skipped.
func moduleSources(t *testing.T, root string) []moduleSource {
	t.Helper()
	fileSet := token.NewFileSet()
	var sources []moduleSource
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		sources = append(sources, moduleSource{path: path, file: file})
		return nil
	}); err != nil {
		t.Fatalf("scan dispatch sources: %v", err)
	}
	return sources
}

// stringConstants maps every package-level string constant of the module - and every `var` bound
// to the same shapes - to its value, keyed `<package>.<name>`, so a statement assembled from a
// shared row shape (`select ` + askRowColumns + askRowFrom + ...) is scanned with those pieces in
// place. The key carries the package because two packages may declare the same name over
// different SQL, and welding their values together would fabricate a statement that exists in
// neither.
//
// A constant is routinely written from another one - api's askRowColumns is docs.AskColumns plus
// its own columns - so the map is resolved to a fixpoint rather than in one pass: every
// declaration is re-read against the previous pass's map until a pass changes nothing. Resolving
// once would leave the inner constant a gap and drop its text from every statement built on the
// outer one. Dropping an ambiguous key can unresolve a chain that named it, so a pass takes keys
// away as well as adding them and the iteration is not monotone; it is bounded by the number of
// declarations and fails rather than spinning.
func stringConstants(t *testing.T, sources []moduleSource) map[string]string {
	t.Helper()
	type declaration struct {
		pkg   string
		key   string
		value ast.Expr
	}
	var declarations []declaration
	for _, source := range sources {
		file := source.file
		for _, declared := range file.Decls {
			general, ok := declared.(*ast.GenDecl)
			if !ok || (general.Tok != token.CONST && general.Tok != token.VAR) {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != len(value.Values) {
					continue
				}
				for index, name := range value.Names {
					declarations = append(declarations, declaration{
						pkg:   file.Name.Name,
						key:   file.Name.Name + "." + name.Name,
						value: value.Values[index],
					})
				}
			}
		}
	}
	constants := map[string]string{}
	for pass := 0; pass <= len(declarations); pass++ {
		resolved := map[string]string{}
		dropped := map[string]struct{}{}
		for _, declared := range declarations {
			text, ok := literalText(declared.value, declared.pkg, constants)
			if !ok {
				continue
			}
			if existing, clash := resolved[declared.key]; clash && existing != text {
				// Several directories declare a package of the same name (main,
				// config, store), so this key is not unique. Two different values
				// under it mean the name cannot be resolved: drop it and let it
				// render as a gap.
				dropped[declared.key] = struct{}{}
				delete(resolved, declared.key)
				continue
			}
			if _, gone := dropped[declared.key]; gone {
				continue
			}
			resolved[declared.key] = text
		}
		if maps.Equal(resolved, constants) {
			return constants
		}
		constants = resolved
	}
	t.Fatalf("string constants did not settle in %d passes", len(declarations)+1)
	return nil
}

// sqlStatements returns every string expression in a parsed Go file - a literal or a
// concatenation of literals and known constants - lowercased with its whitespace collapsed, so
// a clause split across lines or constants reads as one statement.
func sqlStatements(file *ast.File, constants map[string]string) []string {
	var statements []string
	ast.Inspect(file, func(node ast.Node) bool {
		expression, ok := node.(ast.Expr)
		if !ok {
			return true
		}
		text, ok := literalText(expression, file.Name.Name, constants)
		if !ok {
			return true
		}
		statements = append(statements, sqlWhitespace.ReplaceAllString(strings.ToLower(text), " "))
		// The whole concatenation is one statement; its parts are not statements of their own.
		return false
	})
	return statements
}

// literalText renders a string literal, or a `+` chain of literals and constants, as its text.
// A bare identifier resolves in pkg, its own package; a qualified one (docs.AskColumns) resolves
// in the package it names. Inside a `+` chain an operand that resolves to nothing renders as a
// gap, so a query built around a runtime value is still scanned for the parts that are known;
// on its own it is not text at all, so the expression holding it keeps being walked.
func literalText(expression ast.Expr, pkg string, constants map[string]string) (string, bool) {
	switch node := expression.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		text, err := strconv.Unquote(node.Value)
		if err != nil {
			return "", false
		}
		return text, true
	case *ast.Ident:
		text, known := constants[pkg+"."+node.Name]
		return text, known
	case *ast.SelectorExpr:
		qualifier, ok := node.X.(*ast.Ident)
		if !ok {
			// A method call on an expression (tx.QueryRow(...).Scan) is not text, and
			// reporting it as text would prune the call holding the statement.
			return "", false
		}
		text, known := constants[qualifier.Name+"."+node.Sel.Name]
		return text, known
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftOK := literalText(node.X, pkg, constants)
		right, rightOK := literalText(node.Y, pkg, constants)
		if !leftOK && !rightOK {
			return "", false
		}
		// An operand that resolves to nothing - a call, a runtime value, a constant whose
		// name is ambiguous - contributes its own string literals instead, joined by a
		// space, so a clause they spell is still part of the statement rather than a gap
		// that hides it: strings.Join([]string{"for update"}, "") reds, and so does
		// []string{"for", "update"}, which no single literal spells.
		// A spliced gap is padded on both sides, so a fragment can never weld itself to the
		// operand beyond it. Nothing is inserted when both operands resolve, so a keyword
		// split across a concatenation ("for upd" + "ate") stays one word and is refused.
		if !leftOK {
			left = " " + literalsWithin(node.X) + " "
		}
		if !rightOK {
			right = " " + literalsWithin(node.Y) + " "
		}
		return left + right, true
	}
	return "", false
}

// literalsWithin renders every string literal inside an expression the scan could not resolve,
// so the words of a clause built at runtime still reach the statement. It is deliberately loose,
// and that looseness is the second false-positive shape the check's own doc names: a literal that
// never reaches the query still lands in the scanned text, so it can supply either half of a
// match - the keyword or an owner table's name. Such a red is told apart the way the test's doc
// describes it: it reads as a word the statement did not spell.
func literalsWithin(expression ast.Expr) string {
	var parts []string
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if ok && literal.Kind == token.STRING {
			if text, err := strconv.Unquote(literal.Value); err == nil {
				parts = append(parts, text)
			}
		}
		return true
	})
	return strings.Join(parts, " ")
}
