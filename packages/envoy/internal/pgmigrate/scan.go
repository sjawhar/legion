package pgmigrate

import (
	"slices"
	"strings"
)

// The scanner here reads SQL text as Postgres 18 reads it. It does not reason about the grammar:
// scanl.go is a transcription of src/backend/parser/scan.l, rule by rule and state by state, and
// everything in this file is a reader of the tokens that transcription hands out. A reading takes
// one standard_conforming_strings, because a backslash inside a plain literal, and inside the
// N'…' that is one under another name, escapes the byte after it only when the setting is off,
// and because U&'…' is refused outright when it is off. Three readers ask for the setting on, the
// default: checkCensus's reading of a census file (censusTokens), the census's reading of a
// migration (readMigration) and its reading of a migration's lock timeouts
// (migrationLockTimeouts), all of them reading this repository's own migration files, which are
// written with the setting on. redactedQuery, which reads another session's query text and is
// told neither setting, reads that text whole under both.

// sqlTokenKind is the kind of one token scanSQL reads.
type sqlTokenKind int

const (
	sqlSpace            sqlTokenKind = iota // one byte of scan.l's {space}
	sqlComment                              // {whitespace}'s -- comment, or an <xc> /* */, which nests
	sqlString                               // '…', E'…', B'…', X'…' and the '…' an N takes the setting of
	sqlDollarString                         // $$…$$ or $tag$…$tag$
	sqlUnicodeEscape                        // U&'…' or U&"…"
	sqlQuotedIdentifier                     // "…"
	sqlWord                                 // {identifier}: a keyword or an identifier
	sqlNumber                               // {decinteger}, {hexinteger}, {numeric}, {real} and their kin
	sqlParam                                // {param}: $1
	sqlOther                                // {self}, {operator} and anything else one byte wide
)

// sqlToken is one token scanSQL reads, including the decoded name of a quoted identifier.
type sqlToken struct {
	kind       sqlTokenKind
	start, end int
	identifier string
}

// scanSQL reads text token by token as Postgres reads it with standard_conforming_strings on,
// calling visit with each token. It returns the first error visit returns and calls visit no
// further after one. Where scan.l would refuse the text - an unterminated literal, trailing
// junk after a number - the tokens carry on, the readers of a census and a migration wanting
// what is there rather than Postgres's refusal; redactedQuery, which is shown texts Postgres
// has refused, reads the refusal with lexSQL instead.
func scanSQL(text string, visit func(sqlToken) error) error {
	var failure error
	lexSQL(text, true, func(token sqlToken) {
		if failure == nil {
			failure = visit(token)
		}
	})
	return failure
}

// isDigit is scan.l's decdigit, [0-9].
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// isIdentStart is scan.l's ident_start, [A-Za-z\200-\377_], which also starts a dollar-quote tag.
func isIdentStart(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || c >= 0x80
}

// isIdentCont is scan.l's ident_cont, [A-Za-z\200-\377_0-9\$].
func isIdentCont(c byte) bool { return isIdentStart(c) || isDigit(c) || c == '$' }

// hiddenSpans returns, in order and merged where they overlap, the spans of text that a session
// with standard_conforming_strings off does not read as structure: a string, dollar-quoted or
// Unicode-escaped literal, or a comment. That is the reading in which a backslash inside a plain
// literal escapes the byte after it, so that a backslash before a quote carries that literal on
// to the next quote and moves where every literal after it starts - and where every comment after
// it starts with them, since a -- or a /* inside what the other reading is reading as a literal
// opens a comment here, and a comment's text can hold anything.
//
// The tail of a reading scan.l refuses is a span of its own, of either reading: from the byte
// scan.l raised its error on to the end of the text. Postgres read no further, so there are no
// tokens of its there to call structure, and the text that follows is whatever the session was
// about to have read - which is why it is covered rather than printed. onFailure is where the
// default reading was refused, or -1.
func hiddenSpans(text string, onFailure int) []sqlToken {
	var spans []sqlToken
	offFailure := lexSQL(text, false, func(token sqlToken) {
		switch token.kind {
		case sqlString, sqlDollarString, sqlUnicodeEscape, sqlComment:
			spans = append(spans, token)
		}
	})
	for _, failure := range [2]int{onFailure, offFailure} {
		if failure >= 0 {
			spans = append(spans, sqlToken{kind: sqlString, start: failure, end: len(text)})
		}
	}
	slices.SortStableFunc(spans, func(a, b sqlToken) int { return a.start - b.start })
	// Only spans that overlap are merged, so that the loop below always has one span to ask
	// about and a comment which merely touches a literal stays the comment it is.
	merged := spans[:0]
	for _, span := range spans {
		if last := len(merged) - 1; last >= 0 && span.start < merged[last].end {
			if span.end > merged[last].end {
				merged[last].end = span.end
			}
			if span.kind != sqlComment {
				merged[last].kind = span.kind
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

// redactedQuery is a session's query text with its literals, numbers and comments taken out, for a
// report read more widely than the database: each string literal, dollar-quoted literal and
// Unicode-escaped literal or identifier becomes ?, and so does each number, which scan.l reads
// whole with everything that belongs to it (1.5e3, 0x1F), but for a parameter's number ($1);
// each comment becomes one space, and each run of space one space. Keywords, identifiers, quoted
// identifiers, operators and punctuation stay, so the statement's shape and the names it touches
// can be read. A text cut short mid-literal, as pg_stat_activity cuts one at
// track_activity_query_size, loses the rest of that literal with it, since an unterminated
// literal takes the rest of the text.
//
// The holder session owns standard_conforming_strings and its query text does not carry it, so no
// one reading of that text can be trusted: a backslash before a closing quote ends the literal
// under the default reading, the setting on, and carries it on to the next quote under the other,
// which moves where the next literal of the statement starts, and with it where a comment opened
// in the bytes between them starts. A reading chosen literal by literal would therefore resume
// inside a literal or a comment of the reading Postgres used, and print its bytes. So the whole
// text is read twice instead: the tokens are the default reading's, as they always were, and
// hiddenSpans reads the text again as a session with the setting off reads it.
//
// The rule this loop holds is that a byte is printed only where both readings read it as
// structure, which is to say outside every literal and every comment of either. The default
// reading's own literals and numbers each print one ?, its own comments and space one space; and
// a token any part of which the other reading holds inside a literal or a comment prints one ?
// standing for that whole span, after which every token starting before the span's end prints
// nothing. The one number printed as itself is a parameter's, which {param} reads as one token
// that holds no value a row could have put there. What is left to print is structure, or a name -
// a word, a quoted identifier - which is a name under both readings and which this report keeps
// on purpose.
//
// Which of ? and one space a disagreement becomes follows from what each reading saw. The other
// reading holding the bytes inside a literal makes them ?, a literal being where a value is, even
// where this reading reads space or a comment there. The other reading holding them inside a
// comment makes them ? only where this reading would have printed them: over this reading's own
// space or comment it hides nothing that was going to be printed, and those bytes stay the one
// space they have always been, so a comment both readings read as a comment collapses as it did
// before.
//
// A text Postgres refuses is not a text this report may skip: a session that errors inside a
// transaction keeps every lock it took before the error, sits idle in transaction (aborted), and
// pg_stat_activity holds its statement word for word, which is exactly the session a migration
// queues behind. Where scan.l raised its error, Postgres read no further, so from that byte on
// the text is covered by one ? rather than printed, under either reading: where the lexer cannot
// go on the report becomes more conservative, never less.
//
// That rule is the lexer's alone. A refusal the parser raises and the lexer does not - 1..10
// outside a PL/pgSQL for loop, a comma where a name was wanted - moves nothing, because where
// every literal and every comment of a text begins and ends is the lexer's and does not depend
// on the parser reaching it: the rule above, that a byte is printed only where both readings
// read it as structure, survives a parse error untouched. Hiding from a parse error instead
// would need a parser in here, which is a second thing to get wrong. A text such as
// select 1..e'…' '…\' , ' SECRET ' is therefore not hidden by its refusal: PostgreSQL refuses it
// at the .. , which is the parser's refusal and not the lexer's, and what keeps its secret is
// scanl.go reading the number as {numericfail} does - throwing the dots back and returning the
// number - rather than running a number over the dots and the e.
//
// Two properties follow. A query whose literals hold no backslash, and which neither reading is
// refused, is redacted byte for byte as the default reading alone redacts it, because the two
// readings then agree on where every literal and every comment ends. A query whose literal holds
// one can lose shape - a comma, a whole literal, or a comment's one space, swallowed into the
// single ? that covers the other reading's longer literal or the comment that literal's end
// opens - and loses no value.
func redactedQuery(text string) string {
	var tokens []sqlToken
	onFailure := lexSQL(text, true, func(token sqlToken) { tokens = append(tokens, token) })
	spans := hiddenSpans(text, onFailure)
	var out strings.Builder
	space := false
	covered := 0 // the end of the text the ? last written stands for
	at := 0      // the first span of the other reading that can still reach a token
	for _, token := range tokens {
		if token.start < covered {
			// This token's bytes sit inside a ? already written, so it prints nothing at all.
			continue
		}
		for at < len(spans) && spans[at].end <= token.start {
			at++
		}
		// Whether the other reading holds any of this token's bytes inside a literal or a
		// comment, and whether that span is a literal, which is where a value can be.
		hidden := at < len(spans) && spans[at].start < token.end
		hiddenValue := hidden && spans[at].kind != sqlComment
		switch {
		case !hiddenValue && (token.kind == sqlSpace || token.kind == sqlComment):
			// This reading prints none of these bytes itself, so a comment of the other reading
			// over them hides nothing that was going to be printed: they are the one space they
			// are when the two readings agree.
			space = true
		default:
			if space && out.Len() > 0 {
				out.WriteByte(' ')
			}
			space = false
			switch {
			case hidden:
				// The other reading holds these bytes inside a literal or a comment, or neither
				// reading holds any token there because scan.l stopped: one ? stands for all of
				// that span, and every token starting before its end prints nothing.
				out.WriteByte('?')
				covered = max(spans[at].end, token.end)
			case token.kind == sqlString, token.kind == sqlDollarString, token.kind == sqlUnicodeEscape, token.kind == sqlNumber:
				out.WriteByte('?')
			default:
				out.WriteString(text[token.start:token.end])
			}
		}
	}
	return out.String()
}
