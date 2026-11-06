package pgmigrate

// redactedQuery's property, searched for rather than enumerated. The rows of exempt_test.go pin
// the shapes somebody has already thought of; this file goes looking for the ones nobody has.
//
// It exists because the rule was found wrong four times in one place - which literal prefix takes
// which escape rule under which standard_conforming_strings, and where a number ends - each time
// by a reader running a text the rows did not hold, and each time in a reading of scan.l that
// somebody had reasoned out. scan.go no longer holds such a reading: scanl.go is a transcription
// of src/backend/parser/scan.l, so what is left to check is not a second opinion of the grammar
// but the grammar itself, and the only thing that holds it is a real PostgreSQL.
//
// So the two legs of this file ask different questions.
//
// TestRedactionPrintsNoHiddenByte runs the corpus through redactedQuery and checks, text by text,
// that the marker is never printed where the transcription says it is inside a literal, inside a
// comment, or past the byte scan.l refused the text on. That is a check of the redaction loop,
// not of the lexer: it is the leg that catches a loop which leaves scan.l's token boundaries. It
// also checks the other direction where the two readings agree and neither is refused, since
// there the redaction is the default reading's own and a marker neither reading hides must
// survive it.
//
// What that leg cannot catch is a refusal the transcription does not make, because the bytes it
// calls hidden are the ones our own reading hides: where the transcription never refuses there
// is no byte to be past, the marker counts as shape, and the text leaves the hidden class
// without a word. An assertion keyed off our own refusal cannot fail when the fault is that we
// did not refuse, which is why the server leg below keys its coverage clause off the byte the
// server's own lexer stopped on instead.
//
// TestRedactionAgainstServerOutcomes is the oracle, and it holds the transcription itself to a
// real PostgreSQL by comparing OUTCOMES rather than parsing messages. For each text it asks the
// server twice: once as it stands, and once with the marker replaced by ))) , which is three
// bytes like the marker but can never be one token. If the server answers both the same way -
// the same SQLSTATE at the same position, or the same success with the same columns, and the
// same message once each word is blanked out - then nothing the server did depended on those
// bytes being what they are, so they were not a token of its own and redactedQuery may not print
// them. If the answers differ under both settings, the marker was a token of its own under
// both, and nothing may swallow it: that half asks our own reading nothing either, so a reading
// that hides too much is caught by the same oracle as one that hides too little. The leg skips
// without DISPATCH_TEST_DATABASE_URL, which CI's envoy-go lane sets.
//
// Two kinds of text the oracle cannot settle, both counted and reported rather than dropped
// quietly: one whose error sits before the marker, where the parser stopped and nothing the
// server says is evidence about those bytes either way; and one whose marker is inside a quoted
// identifier, which is a name the report keeps on purpose and which a server that never
// resolved it answers for the same whatever it spells.
//
// Texts PostgreSQL refuses are not excluded. A session that errors inside a transaction keeps
// every lock it took before the error and sits idle in transaction (aborted) with the refused
// statement recorded in pg_stat_activity, which is exactly the session a migration queues behind
// and exactly the text redactedQuery is handed, so the refused class is the one class this search
// may not skip. It is checked three ways: a text our reading refuses must be a text the server
// refuses; a text the server's own lexer refuses - by one of the messages scan.l's error rules
// raise, since no outcome tells a lexical refusal from the parser's - must be a text our reading
// refuses; and a marker at or past the byte our reading was refused on must never be printed,
// which is the rule redactedQuery holds for the class: where the lexer cannot go on, everything
// from there is covered.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// --- the corpus ---

// alphabet holds the pieces a text is built from: every shape of quote and prefix scan.l reads
// differently, the bytes that move a reading (a backslash, a quote, a newline, a carriage
// return), the comment openers, the numeric shapes - a digit, a dot, the two dots of
// {numericfail} and the bare e of {real} - and one piece for each refusal no other piece can
// build a text for: the underscore a {decinteger} holds between two digits, which makes
// {integer_junk} out of a number and what follows it; an escape that makes a byte no UTF-8
// string holds; the first half of a surrogate pair, after which <xeu> refuses whatever comes; a
// codepoint addunicode calls no escape value; a parameter number no int32 holds; and a run of
// {op_chars} four of which are the NAMEDATALEN bytes {operator} refuses. A search whose alphabet
// cannot build a refusal says nothing about it, which is how a transcription missing five of
// them was read as green here.
//
// No piece is a byte that is not valid UTF-8 on its own. A query text PostgreSQL has is a string
// in its server encoding, so a text holding one of those is refused before the lexer sees it and
// is not a text pg_stat_activity can hold; an escape inside a literal is how such a byte is
// reached from a text that is one.
var alphabet = []string{
	"'", "''", `\`, `\'`, `"`, "--", "/*", "*/", "\n", " ", "$t$", "$", "1", ",",
	"B'", "X'", "N'", "E'", "U&'", "a", ".", "..", "e",
	"_", "\r", `\xff`, `\ud83d`, `\u0000`, bigParameter, opCharRun,
}

var (
	// bigParameter is a {param} whose number no int32 holds, which {param} refuses with
	// "parameter number too large".
	bigParameter = "$9999999999"
	// opCharRun is a quarter of NAMEDATALEN bytes of {op_chars}, so that a text of four of them
	// is the length {operator} refuses with "operator too long" and a text of fewer is a run it
	// reads.
	opCharRun = strings.Repeat("@", 16)
)

const (
	marker = "zqz"
	// markerReplacement is as long as the marker, so an error position does not move, and is
	// three bytes no lexer can read as one token, so a server that names the token names one of
	// them and a server that echoes content echoes all three. That is what tells "the marker is
	// a token of its own" from "the marker is content the message happens to quote".
	markerReplacement = ")))"
)

// eachText calls visit with every text of up to maxLen pieces of the alphabet, and with the
// marker word inserted at every gap of each when withMarker. It streams rather than collecting,
// so the corpus need not fit in memory.
func eachText(maxLen int, withMarker bool, visit func(string)) int {
	n := 0
	var build func(prefix []string, depth int)
	build = func(prefix []string, depth int) {
		if len(prefix) > 0 {
			if !withMarker {
				n++
				visit(strings.Join(prefix, ""))
			} else {
				for at := 0; at <= len(prefix); at++ {
					parts := make([]string, 0, len(prefix)+1)
					parts = append(parts, prefix[:at]...)
					parts = append(parts, marker)
					parts = append(parts, prefix[at:]...)
					n++
					visit(strings.Join(parts, ""))
				}
			}
		}
		if depth == maxLen {
			return
		}
		for _, piece := range alphabet {
			build(append(prefix, piece), depth+1)
		}
	}
	build(nil, 0)
	return n
}

// --- what one reading of a text holds ---

// reading is what scanl.go's transcription reads a text as at one standard_conforming_strings:
// the spans it does not read as structure, the quoted identifiers it reads as a name, and where
// scan.l refused it, or -1.
type reading struct {
	spans   []sqlToken
	names   []sqlToken
	failure int
}

func readingOf(text string, standardConformingStrings bool) reading {
	var r reading
	r.failure = lexSQL(text, standardConformingStrings, func(token sqlToken) {
		switch token.kind {
		case sqlString, sqlDollarString, sqlUnicodeEscape, sqlComment:
			r.spans = append(r.spans, token)
		case sqlQuotedIdentifier:
			r.names = append(r.names, token)
		}
	})
	return r
}

// isName reports whether this reading holds [from, to) inside a quoted identifier. What is
// inside one is a name, and the report keeps the names a statement touches on purpose, so the
// server answering alike whether that name is spelt one way or another says nothing: it never
// looked the name up.
func (r reading) isName(from, to int) bool {
	for _, span := range r.names {
		if span.start < to && from < span.end {
			return true
		}
	}
	return false
}

// hides reports whether this reading holds any byte of [from, to) inside a literal or a comment,
// or at or past the byte scan.l refused the text on.
func (r reading) hides(from, to int) bool {
	if r.failure >= 0 && r.failure < to {
		return true
	}
	for _, span := range r.spans {
		if span.start < to && from < span.end {
			return true
		}
	}
	return false
}

func (r reading) same(other reading) bool {
	if r.failure != other.failure || len(r.spans) != len(other.spans) {
		return false
	}
	for i := range r.spans {
		if r.spans[i] != other.spans[i] {
			return false
		}
	}
	return true
}

// --- the redaction loop, over the whole corpus ---

// corpusDepth is how many pieces of the alphabet a text of the corpus holds.
const corpusDepth = 4

// TestRedactionPrintsNoHiddenByte checks the invariant redactedQuery exists for: no byte a
// reading holds inside a literal or a comment, and no byte past the one scan.l refused a reading
// on, survives it. Where the two readings agree and neither is refused it checks the other
// direction too: a marker neither hides is shape, and shape is what the report keeps.
func TestRedactionPrintsNoHiddenByte(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a corpus of millions of texts twice over")
	}
	var hiddenTexts, leaked, refusedTexts, agreedTexts, overHidden int
	leaks := map[string]bool{}
	swallowed := map[string]bool{}
	n := eachText(corpusDepth, true, func(text string) {
		at := strings.Index(text, marker)
		on, off := readingOf(text, true), readingOf(text, false)
		if on.failure >= 0 || off.failure >= 0 {
			refusedTexts++
		}
		printed := strings.Contains(redactedQuery(text), marker)
		if on.hides(at, at+len(marker)) || off.hides(at, at+len(marker)) {
			hiddenTexts++
			if printed {
				leaked++
				if len(leaks) < 20 {
					leaks[fmt.Sprintf("%q -> %q", text, redactedQuery(text))] = true
				}
			}
			return
		}
		if on.same(off) && on.failure < 0 {
			agreedTexts++
			if !printed {
				overHidden++
				if len(swallowed) < 20 {
					swallowed[fmt.Sprintf("%q -> %q", text, redactedQuery(text))] = true
				}
			}
		}
	})
	t.Logf("corpus: %d texts of up to %d pieces, %d refused by scan.l under one reading or the other", n, corpusDepth, refusedTexts)
	t.Logf("%d texts hold the marker inside a literal, inside a comment, or past a refusal; %d printed it", hiddenTexts, leaked)
	for e := range leaks {
		t.Logf("  leak: %s", e)
	}
	t.Logf("%d texts both readings read alike and neither is refused and the marker is shape; %d lost it", agreedTexts, overHidden)
	for e := range swallowed {
		t.Logf("  swallowed: %s", e)
	}
	if leaked > 0 {
		t.Errorf("%d texts printed a marker a reading holds inside a literal, inside a comment, or past a refusal", leaked)
	}
	if overHidden > 0 {
		t.Errorf("%d texts lost a marker both readings read as shape", overHidden)
	}
}

// --- the oracle ---

// answer is what the server said about one statement: an error's SQLSTATE, position and message,
// or the columns of a success. message is compared only with the word that varies blanked out.
type answer struct {
	failed   bool
	sqlstate string
	position int32
	message  string
	columns  string
}

// shape is the part of an answer that cannot depend on what the varying bytes spell: whether
// the server refused the statement, with which SQLSTATE and at which byte, and what columns it
// answered with when it did not.
func (a answer) shape() string {
	return fmt.Sprintf("%v/%s/%d/%s", a.failed, a.sqlstate, a.position, a.columns)
}

// agrees reports whether the server answered the two statements the same way: the same shape,
// and the same message once each statement's own varying word is blanked out. A message that
// quotes the bytes back is therefore agreement when it quotes all three of them (the server
// echoed content) and disagreement when it names one token of them (the server lexed them).
func (a answer) agrees(b answer, aWord, bWord string) bool {
	return a.shape() == b.shape() &&
		strings.ReplaceAll(a.message, aWord, "@") == strings.ReplaceAll(b.message, bWord, "@")
}

// ask sends one statement as a simple query, as a client sends the statements pg_stat_activity
// records, and reads what the server answers. It goes through pgconn rather than pgx because
// pgx refuses a simple query on a connection whose standard_conforming_strings is off, and off
// is half of what this test is for.
func ask(ctx context.Context, conn *pgconn.PgConn, sql string) answer {
	var columns strings.Builder
	results := conn.Exec(ctx, sql)
	for results.NextResult() {
		for _, field := range results.ResultReader().FieldDescriptions() {
			fmt.Fprintf(&columns, "%s:%d,", field.Name, field.DataTypeOID)
		}
	}
	err := results.Close()
	if err == nil {
		return answer{columns: columns.String()}
	}
	out := answer{failed: true, message: err.Error()}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		out.sqlstate = pgErr.Code
		out.position = pgErr.Position
		out.message = pgErr.Message
	}
	return out
}

// verdict is what the server said about one text's marker under one standard_conforming_strings.
type verdict struct {
	// token is true where the server's answer changed when the marker was replaced by three
	// bytes no lexer can read as one token: the server read the marker as a token of its own.
	token bool
	// conclusive is false where the server refused the text without placing its refusal at or
	// after the marker: one positioned before it, and one it gave no position for at all, which
	// a scanner_yyerror of <xe> or <xeu> gives (PostgreSQL 18.6 answers select E'\ud83dzqz with
	// "invalid Unicode surrogate pair at or near "z"" and no POSITION field). Nothing such an
	// answer says is evidence about the marker's bytes either way, and the message is no
	// substitute: one that names the first byte of the marker as the character it stopped on
	// changes when the marker changes, which is the same change a text whose marker the server
	// read as a token of its own makes.
	conclusive bool
	// coversFrom is the byte the server's own lexer refused the text on, where that byte is the
	// marker's or earlier, and -1 everywhere else. From that byte Postgres read nothing, so
	// redactedQuery must have covered the marker rather than printed it. This is the one clause
	// of this leg that asks our own reading nothing at all, which is what makes it the clause a
	// refusal the transcription does not make cannot silence.
	coversFrom int
	// name is true where this reading holds the marker inside a quoted identifier, which is a
	// name the report keeps on purpose and which the server need never have looked up.
	name bool
	// failed is true where the server refused the text, and lexical is true where the message
	// it refused with is one of scan.l's own error rules rather than the parser's.
	failed, lexical bool
}

// scanlErrors are the messages scan.l raises from its error rules, read off the rules
// themselves: that is what tells a refusal the lexer owns from one the grammar owns, which no
// outcome can, the parser stopping the statement either way. "invalid Unicode escape",
// "invalid Unicode surrogate pair" and "invalid Unicode escape value" are left out although
// <xe>, <xeu> and addunicode raise all three, because gram.y raises the same words for a U&'…'
// whose escapes it unpicks after the lexer handed the literal over whole - PostgreSQL 18.6
// answers select U&'\' and select E'\u12' alike - so the message cannot say which of the two
// refused the text. "invalid byte sequence for encoding" is in, although a text whose own bytes
// are not a string in the server encoding is refused with it before the lexer runs, because
// every piece of the alphabet is valid UTF-8 and so is every text built from them: in this
// corpus the only way to that message is the pg_verifymbstr of the <xqs> action, over a literal
// an escape made a byte for.
var scanlErrors = []string{
	"unterminated /* comment",
	"unterminated bit string literal",
	"unterminated hexadecimal string literal",
	"unterminated quoted string",
	"unterminated dollar-quoted string",
	"unterminated quoted identifier",
	"zero-length delimited identifier",
	"unsafe use of string constant with Unicode escapes",
	"unsafe use of \\' in a string literal",
	"trailing junk after numeric literal",
	"trailing junk after parameter",
	"invalid hexadecimal integer",
	"invalid octal integer",
	"invalid binary integer",
	"operator too long",
	"parameter number too large",
	"invalid byte sequence for encoding",
}

func probe(ctx context.Context, conn *pgconn.PgConn, text string, ours reading) verdict {
	at := strings.Index(text, marker)
	answered := ask(ctx, conn, text)
	out := verdict{failed: answered.failed, name: ours.isName(at, at+len(marker)), coversFrom: -1}
	for _, message := range scanlErrors {
		if strings.HasPrefix(answered.message, message) {
			out.lexical = true
		}
	}
	refusedAt := -1
	if answered.failed && answered.position > 0 {
		refusedAt = int(answered.position) - 1
	}
	if out.lexical && refusedAt >= 0 && refusedAt < at+len(marker) {
		// The server's own lexer stopped on a byte of the marker or before it, so Postgres read
		// none of the text from there and the marker may not be printed. Asking it about a
		// second statement would say nothing: what it answers for three other bytes past the
		// byte it stopped on is the same nothing.
		out.coversFrom = refusedAt
		return out
	}
	if answered.failed && refusedAt < at+len(marker) {
		// The server refused at a byte of the marker or before it, and the clause above could
		// not attribute that refusal to its lexer, so nothing it said is evidence about these
		// bytes. The comparison below would be worse than nothing here: a message that names the
		// byte the server stopped on names a byte of the marker, so it changes when the marker
		// changes whatever the server made of it - PostgreSQL 18.6 answers select E'\ud83dzqz
		// with "invalid Unicode surrogate pair at or near "z"", one of the two messages gram.y
		// raises in the same words, and the same answer with ")" in it for the replacement. A
		// refusal with no position at all is this case too.
		return out
	}
	out.conclusive = true
	changed := ask(ctx, conn, strings.Replace(text, marker, markerReplacement, 1))
	out.token = !answered.agrees(changed, marker, markerReplacement)
	return out
}

// TestRedactionAgainstServerOutcomes holds the transcription to a real PostgreSQL by outcome,
// never by reading what a message says about the marker: a text whose answer does not change
// when the marker changes is a text whose marker that server never read as a token, and
// redactedQuery may not print it; a text whose answer changes under both settings is one where
// the marker is shape under both, and shape is what the report is for, so it must survive.
// Neither half asks our own reading anything, which is the point: a reading that hides too
// little and a reading that hides too much are both wrong against the same oracle, and a search
// that excused itself with its own model would catch neither. It also checks the two halves of
// the refusal rule: what our reading refuses the server must refuse, and what the server's own
// lexer refuses our reading must refuse.
//
// And where that lexer refused a text on the marker's byte or an earlier one, the text is not
// dropped as one the server stopped too early to answer for: it is the class Rule R exists for,
// so what the server says is where its lexer stopped and what this leg checks is that
// redactedQuery covered the marker from there. That clause reads nothing of our own reading -
// not its refusal, not its spans - which is what makes it the clause that fails when a refusal
// scan.l makes is missing from the transcription altogether.
func TestRedactionAgainstServerOutcomes(t *testing.T) {
	if testing.Short() {
		t.Skip("asks a server about thousands of statements")
	}
	url := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set")
	}
	ctx := context.Background()
	settings := [2]string{"on", "off"}
	var conns [2]*pgconn.PgConn
	for i, setting := range settings {
		config, err := pgconn.ParseConfig(url)
		if err != nil {
			t.Fatalf("parse DISPATCH_TEST_DATABASE_URL: %v", err)
		}
		// The setting is given to the server in the startup packet, as PGOPTIONS gives it: a SET
		// in a statement of its own would do, but a SET in the same simple-query string as the
		// text under test is lexed before it takes effect, which has misled a reader of this
		// code before.
		config.RuntimeParams["options"] = "-c standard_conforming_strings=" + setting
		conn, err := pgconn.ConnectConfig(ctx, config)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close(ctx)
		if running := conn.ParameterStatus("standard_conforming_strings"); running != setting {
			t.Fatalf("standard_conforming_strings is %q, want %q", running, setting)
		}
		conns[i] = conn
	}
	sample := serverSample()
	var asked, leaked, lost, shape, inconclusive, named, refusedByUs, refusalMissing, refusalSpurious int
	var covered, uncovered int
	leaks := map[string]bool{}
	losses := map[string]bool{}
	refusals := map[string]bool{}
	for _, text := range sample {
		printed := strings.Contains(redactedQuery(text), marker)
		var verdicts [2]verdict
		for i, setting := range settings {
			ours := readingOf(text, setting == "on")
			verdicts[i] = probe(ctx, conns[i], text, ours)
			switch {
			case ours.failure >= 0 && !verdicts[i].failed:
				refusalSpurious++
				if len(refusals) < 10 {
					refusals[fmt.Sprintf("scs=%s %q: our reading is refused at %d, the server ran it", setting, text, ours.failure)] = true
				}
			case ours.failure < 0 && verdicts[i].lexical:
				refusalMissing++
				if len(refusals) < 10 {
					refusals[fmt.Sprintf("scs=%s %q: the server's lexer refused it, our reading did not", setting, text)] = true
				}
			}
			if ours.failure >= 0 {
				refusedByUs++
			}
			switch {
			case verdicts[i].name:
				named++
			case verdicts[i].coversFrom >= 0:
				// Rule R, asked of the server: its lexer stopped on this byte, so everything
				// from there is covered, marker and all.
				covered++
				if printed {
					uncovered++
					if len(leaks) < 20 {
						leaks[fmt.Sprintf("scs=%s %q -> %q: the server's own lexer refused it at byte %d, at or before the marker", setting, text, redactedQuery(text), verdicts[i].coversFrom)] = true
					}
				}
			case !verdicts[i].conclusive:
				inconclusive++
			default:
				asked++
			}
			// Safety: one setting is enough. A marker that server never read as a token is a
			// marker inside a literal, inside a comment, or past the byte it stopped on, and
			// none of those may reach the report.
			if verdicts[i].conclusive && !verdicts[i].name && !verdicts[i].token && printed {
				leaked++
				if len(leaks) < 20 {
					leaks[fmt.Sprintf("scs=%s %q -> %q: the server answered the same with %s in its place", setting, text, redactedQuery(text), markerReplacement)] = true
				}
			}
		}
		// Shape: both settings are needed, since a marker one reading holds inside a literal is
		// covered however the other reads it.
		if verdicts[0].conclusive && verdicts[1].conclusive && verdicts[0].token && verdicts[1].token &&
			!verdicts[0].name && !verdicts[1].name {
			shape++
			if !printed {
				lost++
				if len(losses) < 20 {
					losses[fmt.Sprintf("%q -> %q: the server read the marker as a token of its own under both settings", text, redactedQuery(text))] = true
				}
			}
		}
	}
	t.Logf("server: %d of %d (text, setting) pairs settled by outcome, %d settled by the byte the server's own lexer stopped on, %d inconclusive (refused at a byte of the marker or before it with its lexer unnamed, or refused with no position), %d where the marker is inside a quoted identifier", asked, len(settings)*len(sample), covered, inconclusive, named)
	t.Logf("%d (text, setting) pairs our reading refuses; %d the server ran anyway, %d the server's own lexer refused and ours did not", refusedByUs, refusalSpurious, refusalMissing)
	for e := range refusals {
		t.Logf("  refusal: %s", e)
	}
	t.Logf("markers the server never read as a token and redactedQuery printed: %d; markers past the byte its lexer stopped on and printed: %d", leaked, uncovered)
	for e := range leaks {
		t.Logf("  leak: %s", e)
	}
	t.Logf("%d texts hold a marker the server read as a token of its own under both settings; %d lost it", shape, lost)
	for e := range losses {
		t.Logf("  lost: %s", e)
	}
	if leaked > 0 {
		t.Errorf("%d markers printed that the server never read as a token", leaked)
	}
	if uncovered > 0 {
		t.Errorf("%d markers printed at or past the byte the server's own lexer refused the text on", uncovered)
	}
	if lost > 0 {
		t.Errorf("%d markers dropped that the server read as a token of its own under both settings", lost)
	}
	if refusalSpurious > 0 || refusalMissing > 0 {
		t.Errorf("%d refusals our reading invented, %d the server's own lexer raised and our reading did not", refusalSpurious, refusalMissing)
	}
}

// serverSample picks the texts the oracle runs: every sampleStride'th text of the corpus whose
// marker our reading holds hidden, and every sampleStride'th one it holds as shape, so that both
// answers the server can give are asked for and a reading that hides too little is as reachable
// as one that hides too much. The enumeration is deterministic, so the sample is the same on
// every run.
func serverSample() []string {
	const sampleStride = 97
	var sample []string
	hidden, shape := 0, 0
	eachText(corpusDepth, true, func(text string) {
		statement := "select " + text
		at := strings.Index(statement, marker)
		on, off := readingOf(statement, true), readingOf(statement, false)
		if on.hides(at, at+len(marker)) || off.hides(at, at+len(marker)) {
			if hidden%sampleStride == 0 {
				sample = append(sample, statement)
			}
			hidden++
			return
		}
		if shape%sampleStride == 0 {
			sample = append(sample, statement)
		}
		shape++
	})
	return sample
}
