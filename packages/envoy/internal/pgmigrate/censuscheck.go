package pgmigrate

import (
	"errors"
	"fmt"
	"strings"
)

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
