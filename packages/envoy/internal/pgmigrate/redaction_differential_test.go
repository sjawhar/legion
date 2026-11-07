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
// without a word. The server leg below catches that fault with two clauses, and neither is the
// other's spare. The wider one compares the two refusals: a text the server's own lexer refused
// and our reading did not. The narrower one reads nothing of our reading at all, not its
// refusal and not its spans, and asks only that redactedQuery covered the marker from the byte
// the server's lexer stopped on, so no fault in the reading can quiet it. Measured on
// 2026-11-06, with ask reading its results and with actParam's int32 check taken out of
// scanl.go, which loses the refusal {param}'s number too large makes: over the sample that
// leg runs, the comparison of refusals failed on 1,250 (text, setting) pairs and the coverage
// clause on 982. Both of them need the server's message to be read as its lexer's own, which
// is what scanlErrors decides.
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
// string holds; both halves of a surrogate pair - the first, after which <xeu> refuses whatever
// comes, and the second, which closes the pair, once on its own and once carrying the quote
// that ends its literal, because the leaking shape is a first half, a byte that recovers
// nothing, a closing half and the end of the literal, which is five pieces where a text holds
// four; a codepoint addunicode calls no escape value; the 0x, 0o and 0b of {hexfail},
// {octfail} and {binfail}, which no other piece can begin since the only digit here is a 1; a
// parameter number no int32 holds; and a run of {op_chars} four of which are the NAMEDATALEN
// bytes {operator} refuses.
//
// A search whose alphabet cannot build a refusal says nothing about it: a transcription missing
// five of them was read as green here, and this alphabet could form neither {hexfail},
// {octfail}, {binfail} nor a closing surrogate half. What holds the alphabet to them is the
// per-class tally of TestRedactionAgainstServerOutcomes, which counts the hits a text the corpus
// built won apart from the hits the class's own fixed statement won, and fails the test for a
// class no text of the corpus reached. Where the corpus forms a class in too few texts for the
// sample's stride to land on one, the sample takes one of them by name rather than excusing the
// class.
//
// No piece is a byte that is not valid UTF-8 on its own. A query text PostgreSQL has is a string
// in its server encoding, so a text holding one of those is refused before the lexer sees it and
// is not a text pg_stat_activity can hold; an escape inside a literal is how such a byte is
// reached from a text that is one.
var alphabet = []string{
	"'", "''", `\`, `\'`, `"`, "--", "/*", "*/", "\n", " ", "$t$", "$", "1", ",",
	"B'", "X'", "N'", "E'", "U&'", "a", ".", "..", "e",
	"_", "\r", `\xff`, `\ud83d`, `\udc00`, `\udc00'`, `\u0000`, "0x", "0o", "0b", bigParameter, opCharRun,
}

var (
	// bigParameter is a {param} whose number no int32 holds, which {param} refuses with
	// "parameter number too large".
	bigParameter = "$9999999999"
	// opCharRun is a quarter of NAMEDATALEN bytes of {op_chars}, so that a text of four of them
	// is the length {operator} refuses with "operator too long" and a text of fewer is a run it
	// reads.
	opCharRun = strings.Repeat("@", 16)
	// longOperator is four of those pieces: NAMEDATALEN bytes of {op_chars}, the length
	// {operator} refuses, and the only run of that length a text of corpusDepth pieces can
	// hold, since every other piece of the alphabet is shorter than this one or holds a byte
	// that is no {op_chars}.
	longOperator = strings.Repeat(opCharRun, 4)
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
	// The floor the two checks below stand on: they can only fail over texts the corpus still
	// builds, and a corpus that stops building them fails nothing at all. It is taken on
	// hiddenTexts+agreedTexts, the texts this leg checks in one direction or the other.
	// Measured on 2026-11-06 with the alphabet as it stands: 4,080,589 texts hold the marker
	// hidden and 1,721,811 hold it as shape, 5,802,400 of 7,678,370 texts. The floor is about
	// the corpus still forming those two classes rather than about its size, so a round that
	// deliberately shrinks the corpus moves this number in the same commit and measures the
	// two counts again.
	if hiddenTexts+agreedTexts < 500_000 {
		t.Fatalf("the corpus holds the marker hidden in %d texts and as shape in %d, %d together of %d: it has collapsed and these checks have nothing left to fail on", hiddenTexts, agreedTexts, hiddenTexts+agreedTexts, n)
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
//
// Each result is read and closed before the next is asked for. A ResultReader left open holds
// its statement's rows on the connection, and NextResult called over them answers "unexpected
// DataRow message without preceding RowDescription" rather than the next result: that is one
// bogus refusal for every statement the server answered a row to - select 1 among them - with
// no SQLSTATE and no position, which this leg would read as a refusal the grammar made. What that
// cost the oracle, and the floor that now fails a run which compares no success, are recorded at
// that floor rather than here.
func ask(ctx context.Context, conn *pgconn.PgConn, sql string) answer {
	var columns strings.Builder
	var failure error
	results := conn.Exec(ctx, sql)
	for results.NextResult() {
		reader := results.ResultReader()
		for _, field := range reader.FieldDescriptions() {
			fmt.Fprintf(&columns, "%s:%d,", field.Name, field.DataTypeOID)
		}
		// The first error is the one the server gave: a later result's reader answers for a
		// statement the server never reached.
		if _, err := reader.Close(); err != nil && failure == nil {
			failure = err
		}
	}
	err := results.Close()
	if failure == nil {
		failure = err
	}
	if failure == nil {
		return answer{columns: columns.String()}
	}
	out := answer{failed: true, message: failure.Error()}
	var pgErr *pgconn.PgError
	if errors.As(failure, &pgErr) {
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
	// conclusive is false where the server refused the text without placing its refusal after
	// the marker, and the clause below could not read that refusal as the lexer's own: the
	// grammar stopped at or before those bytes, so nothing the server said is evidence about
	// them either way. The message is no substitute: one that names the byte the server stopped
	// on names a byte of the marker, so it changes when the marker changes whatever the server
	// made of those bytes. A refusal the server gave no position for at all is this case too.
	conclusive bool
	// coversFrom is the byte the server's own lexer refused the text on, where that byte is the
	// marker's or earlier, and -1 everywhere else. From that byte Postgres read nothing, so
	// redactedQuery must have covered the marker rather than printed it. This clause asks our
	// own reading nothing at all - not its refusal, not its spans - so no fault in that reading
	// can quiet it. What it is not is the clause that catches most of a refusal the
	// transcription does not make: that is the comparison of the two refusals, by the
	// measurement this file's header records. The two are not one set either way: this clause
	// needs the server's stop byte to reach the marker, the comparison needs only that the
	// server's lexer refused where our reading did not.
	coversFrom int
	// name is true where this reading holds the marker inside a quoted identifier, which is a
	// name the report keeps on purpose and which the server need never have looked up.
	name bool
	// failed is true where the server refused the text; lexical is true where that refusal is
	// one scan.l's own error rules make rather than the parser's, which scanlErrors decides by
	// the words of the message and by the suffix scanner_yyerror gives them; and class is the
	// entry of scanlErrors whose words those are, or nil where the server did not refuse the
	// text or refused it in words no rule of scan.l raises. The tally over the sample counts a
	// hit for a class only where lexical is true, since a grammar refusal in the lexer's words
	// is not the lexer's hit; the exception is a shared class, which the entry itself says it
	// is, where no message tells the two apart and the count holds both.
	failed, lexical bool
	class           *scanlError
}

// scanlError is one of the refusals scan.l makes: the words its rule raises, how it raises them,
// and a statement that makes a PostgreSQL 18.6 raise them. The table below is read off scan.l's
// error rules and off the {hexfail}, {octfail}, {binfail}, {realfail}, {xeunicodefail},
// {integer_junk}, {numeric_junk}, {real_junk} and {param_junk} definitions those rules are
// written for - the set scanl.go's header names - because the words are what tell a refusal the
// lexer owns from one the grammar owns, which no outcome can, the parser stopping the statement
// either way.
type scanlError struct {
	// message is the words the rule raises, with nothing of the position in them.
	message string

	// yyerror is true where scan.l raises the message through yyerror(). scanner_yyerror puts
	// the rest of the buffer from the byte it stopped on after the words,
	//
	//	errmsg("%s at or near \"%s\"", _(message), loc)
	//
	// or ends them with " at end of input" where that byte is the end of the buffer, so a
	// message in these words carrying neither suffix was not raised by the lexer. That is what
	// tells scan.l's "invalid Unicode surrogate pair" and "invalid Unicode escape value" from
	// the two str_udeescape raises in the same words for a U&'…' whose escapes gram.y unpicks
	// after the lexer handed the literal over whole. Measured on the 18.6 this module is read
	// against:
	//
	//	select E'\ud83dX' , 1    ERROR:  invalid Unicode surrogate pair at or near "X"
	//	select U&'\d83dX' , 1    ERROR:  invalid Unicode surrogate pair
	//	select E'\U00110000', 1  ERROR:  invalid Unicode escape value at or near "\U00110000"
	//	select U&'\+110000' , 1  ERROR:  invalid Unicode escape value
	yyerror bool

	// shared is true where the grammar raises the same words in the same form, so that no
	// message tells the two apart and this leg may never read one as the lexer's. "invalid
	// Unicode escape" is the one: scan.l raises it from <xe,xeu>{xeunicodefail} and
	// str_udeescape raises it for a U&'…', both through ereport, so neither carries a suffix
	// and PostgreSQL 18.6 answers select E'\u12' and select U&'\z' alike.
	shared bool

	// sample is a statement holding the marker that makes the server raise this class. It is in
	// the oracle's sample beside the generated corpus, and the tally counts its hits apart from
	// theirs: a generated hit is what says the alphabet reaches the class, a fixed hit only that
	// the class is still raised in these words. Each sample is also held to raising its own
	// class - from the lexer, except for a shared class, where raising it at all is all a
	// message can show - since a fixed text that raises something else stands for nothing and
	// the tally would count it all the same.
	sample string

	// unformable, where it is set, says why no statement this leg sends can make the server
	// raise this class, and the tally asks nothing of it.
	unformable string

	// reaches, where it is set, recognises a text of the corpus that makes the server raise this
	// class, and the sample takes the first text of the enumeration it recognises, beside the
	// stride's own picks. It is for a class the corpus forms in too few texts for a stride of
	// one in sampleStride to land on one of them - "operator too long" is two texts of the
	// corpus's millions - so that the class has a hit of the search's own and the tally can
	// still fail on it. A class the stride already covers needs none, and no test here is
	// trusted: the server decides what the text it picked raised, and a pick that raises
	// nothing leaves the class's generated count at zero and reddens the test.
	reaches func(text string) bool
}

var scanlErrors = []scanlError{
	{message: "unterminated /* comment", yyerror: true, sample: "select /* " + marker},
	{message: "unterminated bit string literal", yyerror: true, sample: "select B'" + marker},
	{message: "unterminated hexadecimal string literal", yyerror: true, sample: "select X'" + marker},
	{message: "unterminated quoted string", yyerror: true, sample: "select '" + marker},
	{message: "unterminated dollar-quoted string", yyerror: true, sample: "select $t$" + marker},
	{message: "unterminated quoted identifier", yyerror: true, sample: `select "` + marker},
	{message: "zero-length delimited identifier", yyerror: true, sample: `select "",` + marker},
	// {xusstart} raises this one through ereport, and only with standard_conforming_strings
	// off, which is one of the two settings every text is asked under.
	{message: "unsafe use of string constant with Unicode escapes", sample: "select U&'" + marker + "'"},
	{message: "unsafe use of \\' in a string literal",
		unformable: "<xe>{xeescape} raises it only with backslash_quote off, and both this server and the transcription take that setting at its default"},
	{message: "trailing junk after numeric literal", yyerror: true, sample: "select 1" + marker},
	{message: "trailing junk after parameter", yyerror: true, sample: "select $1" + marker},
	{message: "invalid hexadecimal integer", yyerror: true, sample: "select 0x," + marker},
	{message: "invalid octal integer", yyerror: true, sample: "select 0o," + marker},
	{message: "invalid binary integer", yyerror: true, sample: "select 0b," + marker},
	{message: "operator too long", yyerror: true, sample: "select " + longOperator + " " + marker,
		reaches: func(text string) bool { return strings.Contains(text, longOperator) }},
	{message: "parameter number too large", yyerror: true, sample: "select " + bigParameter + "," + marker},
	// A text whose own bytes are not a string in the server encoding is refused with this
	// message before the lexer runs, but every piece of the alphabet is valid UTF-8 and so is
	// every text built from them: in this corpus the only way to it is the pg_verifymbstr of
	// the <xqs> action, over a literal an escape made a byte for. It is an ereport of its own
	// and carries no position at all.
	{message: "invalid byte sequence for encoding", sample: `select e'\xff',` + marker},
	// The sample for this one is the shape a reading that refuses nowhere prints the tail of: a
	// first half, a byte <xeu> recovers nothing from, a closing half that puts the literal back
	// together and the quote that ends it, so that a reading which does not refuse at the
	// carriage return reads one whole literal and hands the marker after it to the report,
	// while the server's lexer stopped at that byte. It is a fixed text because the corpus
	// builds exactly one text of that shape, and the sample keeps one text in sampleStride, so
	// whether that one is kept turns on where it falls in the enumeration.
	{message: "invalid Unicode surrogate pair", yyerror: true, sample: "select E'\\ud83d\r\\udc00'" + marker},
	{message: "invalid Unicode escape value", yyerror: true, sample: `select E'\U00110000',` + marker},
	{message: "invalid Unicode escape", shared: true, sample: `select E'\u12',` + marker},
}

// refusalClass returns the entry of scanlErrors whose words the server's message is in - the
// longest of their messages the answer begins with, since "invalid Unicode escape" is a prefix
// of "invalid Unicode escape value" - and whether scan.l's own lexer is what raised it. It
// returns the entry rather than its message so that a caller needs no second table of its own
// to read what the class is: whether it is shared with the grammar, say. An answer in none of
// those words is the grammar's own and belongs to no class, which is a nil entry.
func refusalClass(message string) (*scanlError, bool) {
	found := -1
	for i := range scanlErrors {
		if !strings.HasPrefix(message, scanlErrors[i].message) {
			continue
		}
		if found < 0 || len(scanlErrors[i].message) > len(scanlErrors[found].message) {
			found = i
		}
	}
	if found < 0 {
		return nil, false
	}
	return &scanlErrors[found], scanlErrors[found].raisedByLexer(message)
}

// raisedByLexer reports whether this answer, in this class's words, is one the lexer raised: the
// scanner_yyerror suffix where scan.l raises the class through yyerror, and never where the
// grammar raises the same words in the same form.
func (e scanlError) raisedByLexer(message string) bool {
	if e.shared {
		return false
	}
	if !e.yyerror {
		return true
	}
	return strings.HasSuffix(message, " at end of input") ||
		(strings.Contains(message, " at or near \"") && strings.HasSuffix(message, "\""))
}

// byteOffset turns a position PostgreSQL raised an error at into a byte offset into the text it
// was raised for. POSITION is a 1-based count of characters, not of bytes, while every offset
// this leg compares it against - the marker's, and the spans of our own reading - is a byte
// offset, so one multi-byte character before the error would put the comparison out by the
// bytes that character carries beyond its first. A position past the text's last character is
// the text's length in bytes.
func byteOffset(text string, position int32) int {
	left := int(position) - 1
	for at := range text {
		if left == 0 {
			return at
		}
		left--
	}
	return len(text)
}

func probe(ctx context.Context, conn *pgconn.PgConn, text string, ours reading) verdict {
	at := strings.Index(text, marker)
	answered := ask(ctx, conn, text)
	out := verdict{failed: answered.failed, name: ours.isName(at, at+len(marker)), coversFrom: -1}
	if answered.failed {
		out.class, out.lexical = refusalClass(answered.message)
	}
	refusedAt := -1
	if answered.failed && answered.position > 0 {
		refusedAt = byteOffset(text, answered.position)
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
		// not read that refusal as its lexer's, so nothing it said is evidence about these
		// bytes: what stopped there read none of them. The comparison below would be worse than
		// nothing here, since a message that names the byte the server stopped on changes when
		// the marker changes whatever the server made of it. What this branch holds is the
		// grammar's own refusals - a syntax error, and the two str_udeescape raises in scan.l's
		// words for a U&'…' it unpicks, which carry no scanner_yyerror suffix - and the lexer's
		// own refusals that carry no position, which the pg_verifymbstr of <xqs> gives:
		// PostgreSQL 18.6 answers select e'\xff' with an invalid byte sequence for encoding and
		// no POSITION field.
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
// not its refusal, not its spans - so no fault in the reading can quiet it; it is not, though,
// the clause that catches most of a refusal missing from the transcription, as the measurement
// this file's header records. What both of them rest on is the suffix rule of scanlErrors: take
// scan.l's "invalid Unicode surrogate pair" out of the lexical set and a reading that recovers
// from a carriage return inside E'\ud83d…\udc00' rather than refusing prints the marker that
// follows the literal, with both clauses silent on it.
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
	var covered, uncovered, ran int
	ranTexts := map[string]bool{}
	leaks := map[string]bool{}
	losses := map[string]bool{}
	refusals := map[string]bool{}
	// The tally, one entry per class of scanlErrors, counted apart: generated from the texts
	// the corpus built, fixed from the class's own fixed statement, raised where that fixed
	// statement was seen to raise its own class, and saw for what it raised instead where it
	// did not.
	tally := map[*scanlError]*classTally{}
	for i := range scanlErrors {
		tally[&scanlErrors[i]] = &classTally{}
	}
	for _, entry := range sample {
		text := entry.text
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
			if !verdicts[i].failed {
				ran++
				ranTexts[text] = true
			}
			if class := verdicts[i].class; class != nil && (verdicts[i].lexical || class.shared) {
				hits := tally[class]
				if entry.class == nil {
					hits.generated++
				} else {
					hits.fixed++
					if class == entry.class {
						hits.raised = true
					}
				}
			}
			if entry.class != nil {
				hits := tally[entry.class]
				hits.saw = append(hits.saw, "scs="+setting+": "+raisedWhat(verdicts[i]))
			}
			switch {
			case verdicts[i].coversFrom >= 0:
				// Rule R, asked of the server: its lexer stopped on this byte, so everything
				// from there is covered, marker and all. This case comes before the name one
				// because a name the server's lexer never reached is no name it resolved: it is
				// the tail of a refused statement, and the tail is covered like any other.
				covered++
				if printed {
					uncovered++
					if len(leaks) < 20 {
						leaks[fmt.Sprintf("scs=%s %q -> %q: the server's own lexer refused it at byte %d, at or before the marker", setting, text, redactedQuery(text), verdicts[i].coversFrom)] = true
					}
				}
			case verdicts[i].name:
				named++
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
	t.Logf("%d of those pairs are statements the server ran, the pairs whose answers this leg can compare a success over, and %d different statements among them", ran, len(ranTexts))
	// The floor the tally below stands on, and no replacement for it: the tally fails a class
	// no text of the corpus reached, and this fails a sample that reached almost nothing at
	// all, where the tally of a corpus that no longer builds texts would have nothing left to
	// count. It is taken on asked+covered, the pairs this leg settles - by outcome, and by the
	// byte the server's own lexer stopped on. Measured on 2026-11-07 with the alphabet as it
	// stands and with ask reading its results: 50,180 pairs settled by outcome and 48,730 by
	// the stop byte, 98,910 of 158,364 pairs. The floor is about the corpus still forming the
	// refusal classes rather than about its size, so a round that deliberately shrinks the
	// corpus moves this number in the same commit and measures the two counts again.
	if asked+covered < 10_000 {
		t.Fatalf("the sample settled %d (text, setting) pairs by outcome and %d by the byte the server's own lexer stopped on, %d together: the corpus has collapsed and this leg has nothing left to fail on", asked, covered, asked+covered)
	}
	// Successes the leg compared at all, which nothing else here stands in for: a statement the
	// server RAN is the only pair the token test compares anything over, and refusalSpurious -
	// our reading refusing a text the server ran anyway - cannot fire without one. Until
	// 2026-11-06 ask left each result unread, so every statement the server answered a row to
	// came back as a refusal with no SQLSTATE, every pair was a refusal, and the leg went green
	// over a sample it had compared no success in: this is the check that would have said so on
	// the day it landed.
	//
	// The floor is on the different statements rather than on the pairs: a floor on the pairs
	// could never fail by itself, since ranTexts is filled where ran is counted and the pairs are
	// never the smaller number. Measured on 2026-11-07: 3,431 different statements over 6,264
	// pairs. Four of the sample's own statements are ones the server runs rather than texts the
	// corpus built: the three seeded for that purpose, and U&'zqz'. The other eighteen fixed
	// statements are refusals under both settings by construction, which is what they are for, so
	// this floor stands on the corpus's own reach but for those four.
	//
	// Which question each number answers, since none of the four is a measure of quality: this
	// one is diversity of texts, the floor above it is collapse, the bound below it is a sweep
	// that compared each text against itself, and shape is the per-class tally below. 3,431
	// different statements could still be one shape built from different pieces, and no count
	// tells that apart.
	if len(ranTexts) < 1_000 {
		t.Fatalf("the server ran %d different statements of this sample over %d (text, setting) pairs, under this floor of 1,000 statements, where the sample reached 3,431 over 6,264 when the floor was set: the successes this leg compared are too few a set of texts to say anything about the reading", len(ranTexts), ran)
	}
	// And the pairs bounded from ABOVE, after both floors rather than before them, so that a leg
	// which compared almost nothing is told that by the floors in their own words instead of
	// being told a setting was lost. Every text the server ran it ran under one ask or both, so
	// the pairs can never pass twice the statements, and this bound fires on exactly one state:
	// every statement that ran, ran under both asks, and none ran under exactly one. Reaching it
	// at all now means ran and the statements are both past 1,000, so the vacuous 0 >= 0 that a
	// leg comparing nothing produces cannot arrive here.
	//
	// That state has more than one cause, so the message names what was seen and leaves the cause
	// to its reader. A sweep that lost a setting is one: this leg asks every text once per entry
	// of settings, so a duplicated array asks the same setting twice and every statement the
	// server runs it runs under both asks. A reading that takes the server's successes for
	// refusals is another, and it arrives with settings holding both values.
	//
	// A correct sweep cannot reach it, because the corpus itself builds texts the two settings
	// answer differently - a statement standard_conforming_strings makes the server run under one
	// and refuse under the other. Measured on 2026-11-07: 6,264 pairs over 3,431 statements,
	// which is 2,833 statements run under both settings and 598 run under exactly one, so the
	// bound's margin is 598 pairs, 8.7 per cent below where it fires. That premise lives in the
	// corpus's own reach, not in the fixed set: 597 of those 598 are texts the corpus built, so
	// the bound keeps its reason with any fixed statement removed. What would move it is an
	// alphabet or depth change that stops the two settings disagreeing, which the per-class tally
	// below would show as well.
	//
	// It reports rather than stops, so a reader of a red here sees every other check that fires
	// with it. Measured at this head by driving each fault and reading the set of checks the red
	// names, by name rather than by line, since a reorder renames every line below it: a sweep
	// that loses off (6,796 pairs over 3,398) is caught by this bound, the lost-marker check
	// below with 419 lost, the fixed-text assertion with one class unraised and the per-class
	// tally with one class unformed, since U&'zqz' stops raising its class under off; a sweep
	// that loses on (5,732 over 2,866) is caught by this bound and the lost-marker check alone,
	// with 17 lost, since U&'zqz' still raises its class under off; and a reading that takes the
	// server's successes for refusals (3,484 over 1,742, with settings holding both values) is
	// caught by this bound ALONE. The refusal check fires on none of the three, and neither does
	// the per-connection check of standard_conforming_strings, which compares against the same
	// settings array: what that one catches is a connection that did not take the option it was
	// asked for. So one of the three faults is this bound's alone and another has one other
	// check, which is why it is worth its line.
	if ran >= 2*len(ranTexts) {
		t.Errorf("the server ran %d (text, setting) pairs over %d different statements, exactly twice or more: no statement of this sample ran under one setting only, where the corpus gives 598 of them at the head this was measured at. A sweep that lost a setting does that, and so does a reading that takes the server's successes for refusals", ran, len(ranTexts))
	}
	var unformed, unraised, byName, byStride []string
	t.Logf("a fixed statement is asked under both settings, so it contributes exactly two hits where it raises its class under both and one where only one setting raises it; a class whose hits are its fixed statement's alone is a class the corpus never formed, which is its generated count below reading 0")
	t.Logf("refusals the server raised, by class, counting the hits of the texts the corpus built apart from the hits of the class's own fixed statement. A hit is one the lexer raised; a row marked shared is one the grammar raises in the same words in the same form, where no message tells the two apart and the count holds both:")
	for i := range scanlErrors {
		class := &scanlErrors[i]
		hits := tally[class]
		label := class.message
		if class.shared {
			label += " (shared with the grammar)"
		}
		if class.unformable != "" {
			t.Logf("  %-62s not formed here: %s", label, class.unformable)
			continue
		}
		t.Logf("  %-62s generated %d, fixed %d", label, hits.generated, hits.fixed)
		switch {
		case hits.generated == 0:
			// Said of the run rather than of the table: a class carrying a reaches test is one
			// the stride was known to miss, but a class the stride covered today can stop being
			// covered tomorrow, and reading these two lists off the table rather than off the
			// hits would say the stride covers a class it no longer reaches.
			t.Logf("  %-62s rests on its fixed text, not on the search", "")
			unformed = append(unformed, class.message)
		case class.reaches != nil:
			byName = append(byName, class.message)
		default:
			byStride = append(byStride, class.message)
		}
		if class.sample != "" && !hits.raised {
			unraised = append(unraised, fmt.Sprintf("%q, for %q: %s", class.sample, class.message, strings.Join(hits.saw, "; ")))
		}
	}
	t.Logf("%d classes the sample takes a text of the corpus for by name, the corpus forming too few of them for the stride to land on one: %s", len(byName), strings.Join(byName, "; "))
	t.Logf("%d classes the stride's own picks cover: %s", len(byStride), strings.Join(byStride, "; "))
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
	if len(unraised) > 0 {
		t.Errorf("%d of scanlErrors' fixed texts did not raise their own class, so they stand for nothing the tally counts: %s", len(unraised), strings.Join(unraised, "; "))
	}
	if len(unformed) > 0 {
		t.Errorf("no text the corpus built made the server raise %d of scan.l's refusals, so nothing but their fixed texts stands for them and this search says nothing about them: %s", len(unformed), strings.Join(unformed, "; "))
	}
}

// raisedWhat says what the server made of one text, for the report of a fixed statement that did
// not raise the class it stands for.
func raisedWhat(v verdict) string {
	switch {
	case !v.failed:
		return "ran without error"
	case v.class == nil:
		return "refused in words no rule of scan.l raises"
	case v.lexical:
		return fmt.Sprintf("refused with %q from the lexer", v.class.message)
	default:
		return fmt.Sprintf("refused with %q, which this message does not show the lexer raised", v.class.message)
	}
}

// classTally is what the sample found for one refusal class: the hits of the texts the corpus
// built and the hits of the class's own fixed statement, counted apart since only a generated
// hit says the alphabet reaches the class; whether that fixed statement was seen to raise the
// class it stands for; and what it raised instead where it did not.
type classTally struct {
	generated int
	fixed     int
	raised    bool
	saw       []string
}

// sampleText is one statement the oracle asks the server about, and where it came from: class
// names the refusal class whose fixed statement this is, and is nil both for a text the corpus
// built and for one of the three seeded statements the server runs, neither of which stands for
// a class. The tally counts a class's own fixed statement apart from the rest, since only a hit
// from a text the corpus built says the alphabet reaches that class.
type sampleText struct {
	text  string
	class *scanlError
}

// serverSample picks the statements the oracle runs: the fixed statement of each refusal class
// scanlErrors names, so that a class the corpus happens to miss is asked about all the same; the
// first text of the corpus each class's reaches test recognises, so that a class the corpus
// forms in too few texts for the stride to land on one still has a text of the search's own
// here; and then every sampleStride'th text of the corpus whose marker our reading holds hidden,
// and every sampleStride'th one it holds as shape, so that both answers the server can give are
// asked for and a reading that hides too little is as reachable as one that hides too much. The
// enumeration is deterministic, so the sample is the same on every run.
func serverSample() []sampleText {
	const sampleStride = 97
	var sample []sampleText
	// Three statements the server runs, which make the count of compared successes the sample's
	// own rather than the class table's: a marker inside a literal, one as a column alias, and
	// one in a comment, each of which PostgreSQL answers a row to under either setting. Without
	// them that count would be whatever the class table happens to produce - one statement, since
	// select U&'zqz' runs under standard_conforming_strings on. They are a handful of the pairs
	// and statements the checks above stand on, so what they fix is where the number comes from
	// rather than its size.
	for _, ran := range []string{
		"select '" + marker + "'",
		"select 1 as " + marker,
		"select 1 /* " + marker + " */",
	} {
		sample = append(sample, sampleText{text: ran})
	}
	for i := range scanlErrors {
		if class := &scanlErrors[i]; class.sample != "" {
			sample = append(sample, sampleText{text: class.sample, class: class})
		}
	}
	// strided holds the statements the stride took, so that a statement a reaches test asks
	// for is not asked twice and counted twice where the stride took it anyway.
	strided := map[string]bool{}
	reached := make([]string, len(scanlErrors))
	hidden, shape := 0, 0
	eachText(corpusDepth, true, func(text string) {
		statement := "select " + text
		for i := range scanlErrors {
			if scanlErrors[i].reaches != nil && reached[i] == "" && scanlErrors[i].reaches(statement) {
				reached[i] = statement
			}
		}
		at := strings.Index(statement, marker)
		on, off := readingOf(statement, true), readingOf(statement, false)
		if on.hides(at, at+len(marker)) || off.hides(at, at+len(marker)) {
			if hidden%sampleStride == 0 {
				sample = append(sample, sampleText{text: statement})
				strided[statement] = true
			}
			hidden++
			return
		}
		if shape%sampleStride == 0 {
			sample = append(sample, sampleText{text: statement})
			strided[statement] = true
		}
		shape++
	})
	for _, statement := range reached {
		if statement != "" && !strided[statement] {
			sample = append(sample, sampleText{text: statement})
		}
	}
	return sample
}
