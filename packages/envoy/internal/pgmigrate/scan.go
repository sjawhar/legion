package pgmigrate

import "strings"

// The scanner here reads SQL text as Postgres 18's lexer does (src/backend/parser/scan.l): which
// kind of token starts at each byte, and where a comment, a string literal, a quoted identifier
// or a dollar-quoted literal ends. Inside a literal that does not itself ask for escapes a
// backslash escapes the byte after it only when the reading session's
// standard_conforming_strings is off, and the text does not carry that setting, so nextSQLToken
// takes it as backslashEscapes. Three readers ask for the setting on, the default
// (backslashEscapes false): checkCensus's reading of a census file (censusTokens), the census's
// reading of a migration (readMigration) and its reading of a migration's lock timeouts
// (migrationLockTimeouts), all of them reading this repository's own migration files, which are
// written with the setting on. redactedQuery, which reads another session's query text and is
// told neither setting, reads that text whole under both.

// sqlTokenKind is the kind of one token scanSQL reads.
type sqlTokenKind int

const (
	sqlSpace            sqlTokenKind = iota // one byte of scan.l's space
	sqlComment                              // -- to its line's end, or /* */, which nests
	sqlString                               // '…', or E'…' (backslash escapes), B'…', X'…', N'…'
	sqlDollarString                         // $$…$$ or $tag$…$tag$
	sqlUnicodeEscape                        // U&'…' or U&"…", in either case
	sqlQuotedIdentifier                     // "…"
	sqlWord                                 // a keyword or an identifier
	sqlNumber                               // decinteger, {decdigit}(_?{decdigit})*
	sqlOther                                // any other one byte
)

// sqlToken is one token scanSQL reads, including the decoded name of a quoted identifier.
type sqlToken struct {
	kind       sqlTokenKind
	start, end int
	identifier string
}

// scanSQL reads text token by token, calling visit with each token. It stops at the first error
// visit returns, which it returns. A string literal is continued by whitespace holding a newline
// and another quote; what follows a number is read on its own, as scan.l reads it or refuses it as
// trailing junk. An unterminated literal, comment or quoted identifier takes the rest of the text,
// which Postgres refuses before it runs anything.
func scanSQL(text string, visit func(sqlToken) error) error {
	for i := 0; i < len(text); {
		kind, end, identifier := nextSQLToken(text, i, false)
		if err := visit(sqlToken{kind: kind, start: i, end: end, identifier: identifier}); err != nil {
			return err
		}
		i = end
	}
	return nil
}

// nextSQLToken returns the kind of the token that starts at i, the index after it and, for a
// quoted identifier, the identifier's decoded name. backslashEscapes reads a literal that does
// not itself ask for escapes ('…', B'…', X'…', N'…' and U&'…') as a session with
// standard_conforming_strings off reads it, where a backslash escapes the quote after it and so
// carries the literal on to the next quote; with it false such a literal ends where the default
// setting ends it. An E'…' literal escapes under either setting.
func nextSQLToken(text string, i int, backslashEscapes bool) (sqlTokenKind, int, string) {
	c := text[i]
	var next byte
	if i+1 < len(text) {
		next = text[i+1]
	}
	switch {
	case isSQLSpace(c):
		return sqlSpace, i + 1, ""
	case c == '-' && next == '-':
		return sqlComment, endOfLineComment(text, i), ""
	case c == '/' && next == '*':
		return sqlComment, endOfBlockComment(text, i), ""
	case c == '\'':
		return sqlString, endOfStringLiteral(text, i, backslashEscapes), ""
	case c == '"':
		name, end := quotedIdentifier(text, i)
		return sqlQuotedIdentifier, end, name
	case c == '$':
		if end, ok := endOfDollarQuote(text, i); ok {
			return sqlDollarString, end, ""
		}
	case isDigit(c):
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
		return sqlNumber, end, ""
	case isIdentStart(c):
		var after byte
		if i+2 < len(text) {
			after = text[i+2]
		}
		switch {
		case (c == 'e' || c == 'E') && next == '\'':
			return sqlString, endOfStringLiteral(text, i+1, true), ""
		case strings.IndexByte("bBxXnN", c) >= 0 && next == '\'':
			return sqlString, endOfStringLiteral(text, i+1, backslashEscapes), ""
		case (c == 'u' || c == 'U') && next == '&' && after == '\'':
			return sqlUnicodeEscape, endOfStringLiteral(text, i+2, backslashEscapes), ""
		case (c == 'u' || c == 'U') && next == '&' && after == '"':
			_, end := quotedIdentifier(text, i+2)
			return sqlUnicodeEscape, end, ""
		}
		end := i + 1
		for end < len(text) && (isIdentStart(text[end]) || isDigit(text[end]) || text[end] == '$') {
			end++
		}
		return sqlWord, end, ""
	}
	return sqlOther, i + 1, ""
}

// isSQLSpace is scan.l's space: [ \t\n\r\f\v].
func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
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
		case isSQLSpace(c):
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

// backslashReadingSpans returns, in order, the spans a session with standard_conforming_strings
// off reads as something other than structure: a string, dollar-quoted or Unicode-escaped
// literal, or a comment. That is the reading in which a backslash inside a plain literal escapes
// the byte after it, so that a backslash before a quote carries that literal on to the next quote
// and moves where every literal after it starts - and where every comment after it starts with
// them, since a -- or a /* inside what the other reading is reading as a literal opens a comment
// here, and a comment's text can hold anything.
func backslashReadingSpans(text string) []sqlToken {
	var spans []sqlToken
	for i := 0; i < len(text); {
		kind, end, _ := nextSQLToken(text, i, true)
		switch kind {
		case sqlString, sqlDollarString, sqlUnicodeEscape, sqlComment:
			spans = append(spans, sqlToken{kind: kind, start: i, end: end})
		}
		i = end
	}
	return spans
}

// redactedQuery is a session's query text with its literals, numbers and comments taken out, for a
// report read more widely than the database: each string literal, dollar-quoted literal and
// Unicode-escaped literal or identifier becomes ?, and so does each number with whatever follows
// it unspaced (1.5e3, 0x1F), but for a parameter's number ($1); each comment becomes one space,
// and each run of space one space. Keywords, identifiers, quoted identifiers, operators and
// punctuation stay, so the statement's shape and the names it touches can be read. A text cut
// short mid-literal, as pg_stat_activity cuts one at track_activity_query_size, loses the rest of
// that literal with it, since an unterminated literal takes the rest of the text.
//
// The holder session owns standard_conforming_strings and its query text does not carry it, so no
// one reading of that text can be trusted: a backslash before a closing quote ends the literal
// under the default reading, the setting on, and carries it on to the next quote under the other,
// which moves where the next literal of the statement starts, and with it where a comment opened
// in the bytes between them starts. A reading chosen literal by literal would therefore resume
// inside a literal or a comment of the reading Postgres used, and print its bytes. So the whole
// text is read twice instead: the tokens are the default reading's, as they always were, and
// backslashReadingSpans reads the text again as a session with the setting off reads it.
//
// The rule this loop holds is that a byte is printed only where both readings read it as
// structure, which is to say outside every literal and every comment of either. The default
// reading's own literals and numbers each print one ?, its own comments and space one space; and
// a token any part of which the other reading holds inside a literal or a comment prints one ?
// standing for that whole span, after which every token starting before the span's end prints
// nothing. The one number printed as itself, a parameter's, is printed only where the $ before it
// was printed as itself, so no $ swallowed into a ? carries a number out of one. What is left to
// print is structure, or a name - a word, a quoted identifier - which is a name under both
// readings and which this report keeps on purpose.
//
// Which of ? and one space a disagreement becomes follows from what each reading saw. The other
// reading holding the bytes inside a literal makes them ?, a literal being where a value is, even
// where this reading reads space or a comment there. The other reading holding them inside a
// comment makes them ? only where this reading would have printed them: over this reading's own
// space or comment it hides nothing that was going to be printed, and those bytes stay the one
// space they have always been, so a comment both readings read as a comment collapses as it did
// before.
//
// Two properties follow. A query whose literals hold no backslash is redacted byte for byte as
// the default reading alone redacts it, because the two readings then agree on where every
// literal and every comment ends. A query whose literal holds one can lose shape - a comma, a
// whole literal, or a comment's one space, swallowed into the single ? that covers the other
// reading's longer literal or the comment that literal's end opens - and loses no value.
func redactedQuery(text string) string {
	spans := backslashReadingSpans(text)
	var out strings.Builder
	space := false
	previous := sqlToken{kind: sqlSpace}
	covered := 0 // the end of the text the ? last written stands for
	span := 0    // the first span of the other reading that can still reach a token
	for i := 0; i < len(text); {
		kind, end, _ := nextSQLToken(text, i, false)
		if i < covered {
			// This token's bytes sit inside a ? already written, so it prints nothing at all.
			i = end
			continue
		}
		for span < len(spans) && spans[span].end <= i {
			span++
		}
		// Whether the other reading holds any of this token's bytes inside a literal or a
		// comment, and whether that span is a literal, which is where a value can be.
		hidden := span < len(spans) && spans[span].start < end
		hiddenValue := hidden && spans[span].kind != sqlComment
		switch {
		case !hiddenValue && (kind == sqlSpace || kind == sqlComment):
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
				// The other reading holds these bytes inside a literal or a comment: one ?
				// stands for all of that span, and every token starting before its end prints
				// nothing.
				out.WriteByte('?')
				covered = max(spans[span].end, end)
			case kind == sqlNumber && previous.kind == sqlOther && previous.end == i && text[i-1] == '$':
				out.WriteString(text[i:end])
			case kind == sqlNumber:
				for end < len(text) && (isIdentStart(text[end]) || isDigit(text[end]) || text[end] == '.') {
					end++
				}
				out.WriteByte('?')
			case kind == sqlString || kind == sqlDollarString || kind == sqlUnicodeEscape:
				out.WriteByte('?')
			default:
				out.WriteString(text[i:end])
				// previous is the token last printed as itself. A token printed as ? or left to
				// a space leaves it behind, and no token fits between a token and one starting
				// where it ends, so the $ a parameter's number is printed after is always the $
				// immediately before it and always one printed as itself.
				previous = sqlToken{kind: kind, start: i, end: end}
			}
		}
		i = end
	}
	return out.String()
}
