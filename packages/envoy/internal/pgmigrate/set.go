// Package pgmigrate holds what Envoy's two Postgres migration runners share: Dispatch's
// (internal/dispatch/store) and the secrets broker's (internal/broker/store). Both record an
// applied migration by its version alone and apply every version they have not recorded, so both
// trust the set they embed to give each version one file, and both run a migration's statements
// under locks that production's reads and writes queue behind. Load refuses a set either runner
// would apply other than as written, before the runner opens a transaction; Exec applies each
// migration with its lock waits bounded by LockTimeout, and names the lock a migration gave up on
// and the sessions that held it; Census reads, before a deployment applies the migrations a
// database has not, what they would lock and the rows their own censuses count, and writes
// nothing.
package pgmigrate

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Migration is one numbered forward migration.
type Migration struct {
	Version int
	Name    string
	SQL     string
	// Census is the text of <version>_<name>.census.sql when the migration declares one: one
	// select answering one integer, the number of existing rows the migration would refuse or
	// rewrite, which a deployment reads with Census before it applies the migration. Empty when
	// the migration declares none. No runner runs it.
	Census string
	// CensusName is the census file's name, for messages; empty when Census is.
	CensusName string
}

// Load reads every entry of dir in fsys and returns the forward migrations in version order.
//
// An entry is <version>_<name>.up.sql, a forward migration, <version>_<name>.down.sql, a
// rollback script an operator runs by hand and no runner applies, or <version>_<name>.census.sql,
// the migration's census (Migration.Census), which a deployment reads with Census and no runner
// runs. <version> is decimal digits naming a number from 1 to the largest the runners' integer
// version column holds. Load refuses the whole set, naming the files, when:
//
//   - two forward migrations share a version: a runner records a migration by its version, so it
//     would apply the first, pass over the rest as already applied, and report success;
//   - an entry is named any other way: a runner reading only *.up.sql would never see it, while
//     whoever wrote it believes it runs;
//   - a rollback script or a census has no forward migration of its name;
//   - a census is not one select, holds a Unicode escape, or names pg_terminate_backend,
//     pg_cancel_backend, pg_sleep, an advisory-lock function or a function that runs a query
//     given as text (checkCensus);
//   - a forward migration or a census cannot be read.
//
// It reports every problem it finds and returns no migration when it finds one, so a runner that
// calls it before opening a transaction applies nothing from a set it refuses. The order is the
// versions', never the file names': 9_a.up.sql runs before 0010_b.up.sql. Versions need not be
// contiguous for a runner to apply them; that a set skips no number is the repository's rule
// about handing numbers out, which pgmigratetest.CheckNumberedOneToN holds both stores' sets to.
func Load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("list migrations in %s: %w", dir, err)
	}
	present := make(map[string]bool, len(entries))
	for _, entry := range entries {
		present[entry.Name()] = true
	}
	var problems []error
	migrations := make([]Migration, 0, len(entries))
	// A census can be listed before its migration, so the texts are kept by stem and attached
	// once every migration is read.
	censuses := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			problems = append(problems, fmt.Errorf("%s is a directory, not a migration named <version>_<name>.up.sql", name))
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".census.sql"); ok {
			if !present[stem+".up.sql"] {
				problems = append(problems, fmt.Errorf("census %s has no forward migration %s.up.sql", name, stem))
				continue
			}
			text, err := fs.ReadFile(fsys, path.Join(dir, name))
			if err != nil {
				problems = append(problems, fmt.Errorf("read census %s: %w", name, err))
				continue
			}
			if err := checkCensus(string(text)); err != nil {
				problems = append(problems, fmt.Errorf("census %s: %w", name, err))
				continue
			}
			censuses[stem] = string(text)
			continue
		}
		if stem, ok := strings.CutSuffix(name, ".down.sql"); ok {
			if !present[stem+".up.sql"] {
				problems = append(problems, fmt.Errorf("rollback script %s has no forward migration %s.up.sql", name, stem))
			}
			continue
		}
		stem, ok := strings.CutSuffix(name, ".up.sql")
		if !ok {
			problems = append(problems, fmt.Errorf(
				"%s is not named <version>_<name>.up.sql, <version>_<name>.down.sql or <version>_<name>.census.sql, so no runner would apply it", name))
			continue
		}
		version, err := parseVersion(stem)
		if err != nil {
			problems = append(problems, fmt.Errorf("migration %s: %w", name, err))
			continue
		}
		sql, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			problems = append(problems, fmt.Errorf("read migration %s: %w", name, err))
			continue
		}
		migrations = append(migrations, Migration{Version: version, Name: name, SQL: string(sql)})
	}
	for i := range migrations {
		stem := strings.TrimSuffix(migrations[i].Name, ".up.sql")
		if text, ok := censuses[stem]; ok {
			migrations[i].Census = text
			migrations[i].CensusName = stem + ".census.sql"
		}
	}
	// Stable, so files that share a version stay in name order for the refusal that names them.
	slices.SortStableFunc(migrations, func(a, b Migration) int { return a.Version - b.Version })
	for start := 0; start < len(migrations); {
		end := start + 1
		for end < len(migrations) && migrations[end].Version == migrations[start].Version {
			end++
		}
		if end-start > 1 {
			problems = append(problems, fmt.Errorf(
				"migrations %s share version %d: a runner records a migration by its version, so it would apply the first and skip the rest as already applied; keep the number on the file that merged to main first, since a database may already have applied it, and renumber the rest to the next free numbers on main",
				joinNames(migrations[start:end]), migrations[start].Version))
		}
		start = end
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}
	return migrations, nil
}

// checkCensus refuses a census that is not one select: the deployment runs it inside a read-only
// transaction and requires one integer back (Census), so anything else fails there too, but a set
// that cannot be run as written is refused here, before any runner opens a transaction, as the
// rest of Load's rules are. It also refuses the functions a read-only transaction does not stop
// and no census needs: pg_terminate_backend and pg_cancel_backend (the census runs as the
// service's own role, so either ends the live service's sessions), pg_sleep, the advisory-lock
// family, and the functions that run a query given as text, whose literal it does not read. A
// name counts bare or quoted and in any case, wherever it stands outside a comment or a string
// literal, and the census is read as Postgres reads it (censusTokens), so neither can hide a call.
// A census is production-executed code; this is a tripwire, and review is the control.
func checkCensus(text string) error {
	tokens, err := censusTokens(text)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		return errors.New("a census is one select answering one integer; the file holds no statement")
	}
	if first := tokens[0]; first.quoted || (first.text != "select" && first.text != "with") {
		return errors.New("a census is one select answering one integer; the file's statement is not a select")
	}
	for _, token := range tokens {
		for _, forbidden := range censusForbiddenFunctions {
			if strings.HasPrefix(token.text, forbidden) {
				return fmt.Errorf("a census may not call %s: it reads counts and nothing else", token.text)
			}
		}
	}
	return nil
}

// censusForbiddenFunctions are the names, or the families' prefixes, checkCensus refuses: each is
// matched against the start of every word of the census's code. query_to_xml (and
// query_to_xmlschema, query_to_xml_and_xmlschema), ts_stat and ts_rewrite run a query given as
// text, which checkCensus reads as a literal.
var censusForbiddenFunctions = []string{
	"pg_terminate_backend", "pg_cancel_backend", "pg_sleep", "pg_advisory_", "pg_try_advisory_",
	"query_to_xml", "ts_stat", "ts_rewrite",
}

// censusToken is one token of a census's code: a word (a keyword, an identifier or a number) or a
// quoted identifier's name, lowercased, or one other character.
type censusToken struct {
	text   string
	quoted bool
}

// censusTokens reads a census as Postgres 16's lexer (src/backend/parser/scan.l) reads it with
// standard_conforming_strings on, which Census sets, and returns its code: everything outside its
// comments and string literals. A -- comment runs to the end of its line and /* */ comments nest.
// A string literal is '…', E'…' (backslash escapes), B'…', X'…' or N'…', continued by whitespace
// holding a newline and another quote, or dollar-quoted. A Unicode escape, U&'…' or U&"…", is
// refused outright: its escapes can spell any name, and no census needs one. An unterminated
// literal, comment or quoted identifier takes the rest of the text, which Postgres refuses before
// it runs anything.
func censusTokens(text string) ([]censusToken, error) {
	var tokens []censusToken
	for i := 0; i < len(text); {
		c := text[i]
		var next byte
		if i+1 < len(text) {
			next = text[i+1]
		}
		switch {
		case isSQLSpace(c):
			i++
		case c == '-' && next == '-':
			i = endOfLineComment(text, i)
		case c == '/' && next == '*':
			i = endOfBlockComment(text, i)
		case c == '\'':
			i = endOfStringLiteral(text, i, false)
		case c == '"':
			name, end := quotedIdentifier(text, i)
			tokens = append(tokens, censusToken{text: strings.ToLower(name), quoted: true})
			i = end
		case c == '$':
			end, ok := endOfDollarQuote(text, i)
			if !ok {
				tokens = append(tokens, censusToken{text: "$"})
				end = i + 1
			}
			i = end
		case isDigit(c):
			// decinteger, {decdigit}(_?{decdigit})*; what follows is read on its own, as scan.l
			// reads it or refuses it as trailing junk.
			end := i + 1
			for end < len(text) {
				if isDigit(text[end]) {
					end++
				} else if text[end] == '_' && end+1 < len(text) && isDigit(text[end+1]) {
					end += 2
				} else {
					break
				}
			}
			tokens = append(tokens, censusToken{text: text[i:end]})
			i = end
		case isIdentStart(c):
			switch {
			case (c == 'u' || c == 'U') && next == '&':
				return nil, errors.New("a census may not hold a Unicode escape (U&): its escapes can spell any name, and a census needs none")
			case (c == 'e' || c == 'E') && next == '\'':
				i = endOfStringLiteral(text, i+1, true)
			case strings.IndexByte("bBxXnN", c) >= 0 && next == '\'':
				i = endOfStringLiteral(text, i+1, false)
			default:
				end := i + 1
				for end < len(text) && (isIdentStart(text[end]) || isDigit(text[end]) || text[end] == '$') {
					end++
				}
				tokens = append(tokens, censusToken{text: strings.ToLower(text[i:end])})
				i = end
			}
		default:
			tokens = append(tokens, censusToken{text: text[i : i+1]})
			i++
		}
	}
	return tokens, nil
}

// isSQLSpace is scan.l's space: [ \t\n\r\f].
func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isIdentStart is scan.l's ident_start, [A-Za-z\200-\377_], which also starts a dollar-quote tag.
func isIdentStart(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || c >= 0x80
}

// endOfLineComment returns where the -- comment at i ends: at its newline, which it leaves.
func endOfLineComment(text string, i int) int {
	if end := strings.IndexAny(text[i:], "\n\r"); end >= 0 {
		return i + end
	}
	return len(text)
}

// endOfBlockComment returns the index after the */ closing the /* comment at i, counting nested
// ones.
func endOfBlockComment(text string, i int) int {
	depth := 1
	for i += 2; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(text[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(text)
}

// endOfStringLiteral returns the index after the string literal whose opening quote is at i: a
// doubled quote is a quote inside it, a backslash escapes the next byte when backslashEscapes
// (E'…'), and a closing quote followed by quoteContinues' whitespace and another quote continues
// the literal in the same syntax.
func endOfStringLiteral(text string, i int, backslashEscapes bool) int {
	for i++; i < len(text); {
		switch {
		case text[i] == '\\' && backslashEscapes:
			i += 2
		case text[i] != '\'':
			i++
		case i+1 < len(text) && text[i+1] == '\'':
			i += 2
		default:
			continued, ok := quoteContinues(text, i+1)
			if !ok {
				return i + 1
			}
			i = continued + 1
		}
	}
	return len(text)
}

// quoteContinues reports whether a string literal closed just before i continues, and at which
// quote: scan.l's quotecontinue, whitespace holding at least one newline (with -- comments, each
// ending at its newline) and then a quote.
func quoteContinues(text string, i int) (int, bool) {
	newline := false
	for i < len(text) {
		switch c := text[i]; {
		case c == '\n' || c == '\r':
			newline = true
			i++
		case c == ' ' || c == '\t' || c == '\f':
			i++
		case c == '-' && i+1 < len(text) && text[i+1] == '-':
			i = endOfLineComment(text, i)
		case c == '\'' && newline:
			return i, true
		default:
			return 0, false
		}
	}
	return 0, false
}

// quotedIdentifier returns the name of the quoted identifier at i, "" inside it being a quote,
// and the index after its closing quote.
func quotedIdentifier(text string, i int) (string, int) {
	for end := i + 1; end < len(text); end++ {
		if text[end] != '"' {
			continue
		}
		if end+1 < len(text) && text[end+1] == '"' {
			end++
			continue
		}
		return strings.ReplaceAll(text[i+1:end], `""`, `"`), end + 1
	}
	return strings.ReplaceAll(text[i+1:], `""`, `"`), len(text)
}

// endOfDollarQuote returns the index after the dollar-quoted literal whose delimiter, $$ or
// $tag$, starts at i, and false when no delimiter starts there (a parameter such as $1, or a
// lone $). The literal ends at the first repeat of its delimiter.
func endOfDollarQuote(text string, i int) (int, bool) {
	end := i + 1
	if end < len(text) && isIdentStart(text[end]) {
		for end++; end < len(text) && (isIdentStart(text[end]) || isDigit(text[end])); end++ {
		}
	}
	if end >= len(text) || text[end] != '$' {
		return 0, false
	}
	delimiter := text[i : end+1]
	closing := strings.Index(text[end+1:], delimiter)
	if closing < 0 {
		return len(text), true
	}
	return end + 1 + closing + len(delimiter), true
}

// stripSQLComments removes -- comments to the end of their line and /* */ blocks. It does not
// read string literals, so a -- or /* inside one is removed as a comment would be: TouchedTables'
// reading of the repository's own sets is held to the locks each migration takes by
// pgmigratetest.CheckTouchedTablesAgainstLocks.
func stripSQLComments(sql string) string {
	sql = blockComment.ReplaceAllString(sql, " ")
	return lineComment.ReplaceAllString(sql, "")
}

var (
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`--[^\n]*`)
)

// parseVersion reads the version a forward migration's stem begins with.
func parseVersion(stem string) (int, error) {
	digits, _, ok := strings.Cut(stem, "_")
	if !ok || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, errors.New("its name does not begin with a version: <version>_<name>.up.sql, <version> decimal digits")
	}
	version, err := strconv.ParseInt(digits, 10, 32)
	if err != nil || version < 1 {
		return 0, fmt.Errorf("version %s is not a number from 1 to %d", digits, math.MaxInt32)
	}
	return int(version), nil
}

// joinNames lists the migrations' file names for a sentence: a and b, or a, b and c.
func joinNames(migrations []Migration) string {
	names := make([]string, len(migrations))
	for i, migration := range migrations {
		names[i] = migration.Name
	}
	last := len(names) - 1
	return strings.Join(names[:last], ", ") + " and " + names[last]
}
