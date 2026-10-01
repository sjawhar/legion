package pgmigrate

import "strings"

// The helpers here read SQL text as Postgres 16's lexer does (src/backend/parser/scan.l) with
// standard_conforming_strings on: where a comment, a string literal, a quoted identifier or a
// dollar-quoted literal ends. Two readers share them: checkCensus's reading of a census file
// (censusTokens) and the census's reading of a migration (migrationCode).

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
