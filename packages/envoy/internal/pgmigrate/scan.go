package pgmigrate

import "strings"

// The scanner here reads SQL text as Postgres 16's lexer does (src/backend/parser/scan.l) with
// standard_conforming_strings on: which kind of token starts at each byte, and where a comment, a
// string literal, a quoted identifier or a dollar-quoted literal ends. Two readers share it:
// checkCensus's reading of a census file (censusTokens) and the census's reading of a migration
// (migrationCode).

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

// scanSQL reads text token by token, calling visit with each token's kind, span and, for a quoted
// identifier, its decoded name. It stops at the first error visit returns, which it returns. A
// string literal is continued by whitespace holding a newline and another quote; what follows a
// number is read on its own, as scan.l reads it or refuses it as trailing junk. An unterminated
// literal, comment or quoted identifier takes the rest of the text, which Postgres refuses before
// it runs anything.
func scanSQL(text string, visit func(kind sqlTokenKind, start, end int, identifier string) error) error {
	for i := 0; i < len(text); {
		kind, end, identifier := nextSQLToken(text, i)
		if err := visit(kind, i, end, identifier); err != nil {
			return err
		}
		i = end
	}
	return nil
}

// nextSQLToken returns the kind of the token that starts at i, the index after it and, for a
// quoted identifier, the identifier's decoded name.
func nextSQLToken(text string, i int) (sqlTokenKind, int, string) {
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
		return sqlString, endOfStringLiteral(text, i, false), ""
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
			return sqlString, endOfStringLiteral(text, i+1, false), ""
		case (c == 'u' || c == 'U') && next == '&' && after == '\'':
			return sqlUnicodeEscape, endOfStringLiteral(text, i+2, false), ""
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
