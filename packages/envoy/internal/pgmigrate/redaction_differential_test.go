package pgmigrate

// redactedQuery's property, searched for rather than enumerated. The rows of exempt_test.go pin
// the shapes somebody has already thought of; this file goes looking for the ones nobody has.
//
// It exists because the rule was found wrong three times in one place - the table of which
// literal prefix takes which escape rule under which standard_conforming_strings - each time by
// a reader running a text the table did not hold. scan.go reads a holder session's query text
// under both settings and prints a byte only where both readings read it as structure, so the
// property is closed and checkable: no byte inside a literal or a comment of either real reading
// may survive.
//
// The model below is written from PostgreSQL's own src/backend/parser/scan.l - its xq, xe, xb,
// xh, xus and xdolq states, quotestop, quotecontinue, quotefail, nested block comments,
// {xnstart}'s yyless(1) and {integer_junk} - rather than from scan.go, so the two are independent
// readings of the same grammar and a shared mistake is unlikely. TestDifferentialModelAgainstServer
// holds the model itself to a real PostgreSQL, one-directionally: a marker the model says is
// hidden must never be a token the server names. That leg skips without
// DISPATCH_TEST_DATABASE_URL, which CI's envoy-go lane sets.
//
// A text PostgreSQL refuses outright - trailing junk after a numeric literal, U&'…' with the
// setting off - has no reading to be faithful to, and pg_stat_activity holds such texts, so the
// search excludes them and says how many it excluded. What redactedQuery prints there is
// unconstrained by the rule.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// --- the model, written from src/backend/parser/scan.l rather than from scan.go ---

type mspan struct {
	start, end int
	comment    bool
}

const (
	mOK           = iota
	mUnterminated // scan.l refuses it, but a truncated pg_stat_activity text looks like this
	mJunk         // trailing junk after a numeric literal: scan.l refuses before any reading
)

func mIdentStart(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || c >= 0x80
}
func mIdentCont(c byte) bool { return mIdentStart(c) || '0' <= c && c <= '9' || c == '$' }
func mDigit(c byte) bool     { return '0' <= c && c <= '9' }
func mHorizSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\f' || c == '\v'
}
func mSpace(c byte) bool { return mHorizSpace(c) || c == '\n' || c == '\r' }

// mQuoteContinue implements scan.l's quotecontinue:
//
//	{quote}{whitespace_with_newline}{quote}, whitespace_with_newline
//	({horiz_whitespace}*{newline}{whitespace}*), horiz_whitespace ({horiz_space}|{comment}),
//	whitespace ({space}+|{comment}), comment ("--"{non_newline}*)
//
// i is the index just after the closing quote; it returns the index of the quote that carries the
// literal on.
func mQuoteContinue(text string, i int) (int, bool) {
	for i < len(text) { // {horiz_whitespace}*
		if mHorizSpace(text[i]) {
			i++
		} else if strings.HasPrefix(text[i:], "--") {
			for i < len(text) && text[i] != '\n' && text[i] != '\r' {
				i++
			}
		} else {
			break
		}
	}
	if i >= len(text) || (text[i] != '\n' && text[i] != '\r') { // {newline}
		return 0, false
	}
	i++
	for i < len(text) { // {whitespace}*
		if mSpace(text[i]) {
			i++
		} else if strings.HasPrefix(text[i:], "--") {
			for i < len(text) && text[i] != '\n' && text[i] != '\r' {
				i++
			}
		} else {
			break
		}
	}
	if i < len(text) && text[i] == '\'' {
		return i, true
	}
	return 0, false
}

// mQuoted reads the literal whose opening quote is at i. escape is scan.l's xe backslash rule,
// double its xqdouble rule.
func mQuoted(text string, i int, escape, double bool) (int, bool) {
	for j := i + 1; j < len(text); {
		switch {
		case escape && text[j] == '\\':
			j += 2
		case text[j] != '\'':
			j++
		case double && j+1 < len(text) && text[j+1] == '\'':
			j += 2
		default:
			if k, ok := mQuoteContinue(text, j+1); ok {
				j = k + 1
				continue
			}
			return j + 1, true
		}
	}
	return len(text), false
}

// modelSpans reads text as scan.l does with standard_conforming_strings scsOn, returning the
// spans it reads as a literal or a comment and how scan.l ends: mOK, mUnterminated or mJunk.
func modelSpans(text string, scsOn bool) ([]mspan, int) {
	var spans []mspan
	state := mOK
	lit := func(start, end int, ok bool) int {
		spans = append(spans, mspan{start: start, end: end})
		if !ok && state == mOK {
			state = mUnterminated
		}
		return end
	}
	for i := 0; i < len(text); {
		c := text[i]
		var next, after byte
		if i+1 < len(text) {
			next = text[i+1]
		}
		if i+2 < len(text) {
			after = text[i+2]
		}
		switch {
		case c == '-' && next == '-':
			j := i + 2
			for j < len(text) && text[j] != '\n' && text[j] != '\r' {
				j++
			}
			spans = append(spans, mspan{start: i, end: j, comment: true})
			i = j
		case c == '/' && next == '*':
			depth, j := 1, i+2
			for j < len(text) {
				if strings.HasPrefix(text[j:], "/*") {
					depth++
					j += 2
				} else if strings.HasPrefix(text[j:], "*/") {
					depth--
					j += 2
					if depth == 0 {
						break
					}
				} else {
					j++
				}
			}
			if depth != 0 && state == mOK {
				state = mUnterminated
			}
			spans = append(spans, mspan{start: i, end: j, comment: true})
			i = j
		case c == '\'':
			// {xqstart}: BEGIN(scsOn ? xq : xe).
			end, ok := mQuoted(text, i, !scsOn, true)
			i = lit(i, end, ok)
		case c == '"':
			// A quoted identifier is a name, not a literal: no span.
			j := i + 1
			for j < len(text) {
				if text[j] != '"' {
					j++
				} else if j+1 < len(text) && text[j+1] == '"' {
					j += 2
				} else {
					j++
					break
				}
			}
			i = j
		case c == '$':
			j := i + 1
			if j < len(text) && mIdentStart(text[j]) {
				for j++; j < len(text) && (mIdentStart(text[j]) || mDigit(text[j])); j++ {
				}
			}
			if j < len(text) && text[j] == '$' {
				delimiter := text[i : j+1]
				if closing := strings.Index(text[j+1:], delimiter); closing >= 0 {
					i = lit(i, j+1+closing+len(delimiter), true)
				} else {
					i = lit(i, len(text), false)
				}
				continue
			}
			// {dolqfailed} throws back all but the $, and a param is {dollar}{integer}.
			j = i + 1
			for j < len(text) && mDigit(text[j]) {
				j++
			}
			if j > i+1 && j < len(text) && mIdentStart(text[j]) && state == mOK {
				state = mJunk // {param_junk}
			}
			i = j
		case mDigit(c):
			j := i
			for j < len(text) && (mDigit(text[j]) || text[j] == '_' || text[j] == '.') {
				j++
			}
			if j < len(text) && mIdentStart(text[j]) && state == mOK {
				// {integer_junk}/{numeric_junk}: scan.l refuses the text here, so neither
				// reading of what follows is the one Postgres used.
				state = mJunk
			}
			i = j
		case mIdentStart(c):
			switch {
			case (c == 'e' || c == 'E') && next == '\'': // {xestart}
				end, ok := mQuoted(text, i+1, true, true)
				i = lit(i, end, ok)
			case (c == 'b' || c == 'B') && next == '\'': // {xbstart}, xbinside [^']*
				end, ok := mQuoted(text, i+1, false, false)
				i = lit(i, end, ok)
			case (c == 'x' || c == 'X') && next == '\'': // {xhstart}, xhinside [^']*
				end, ok := mQuoted(text, i+1, false, false)
				i = lit(i, end, ok)
			case (c == 'n' || c == 'N') && next == '\'': // {xnstart} eats only the n, and the
				// literal after it takes the setting; the n is counted into the span so that a
				// prefix byte is not a disagreement of its own.
				end, ok := mQuoted(text, i+1, !scsOn, true)
				i = lit(i, end, ok)
			case (c == 'u' || c == 'U') && next == '&' && after == '\'': // {xusstart}
				end, ok := mQuoted(text, i+2, false, true)
				i = lit(i, end, ok)
			case (c == 'u' || c == 'U') && next == '&' && after == '"': // {xuistart}, a name
				j := i + 3
				for j < len(text) {
					if text[j] != '"' {
						j++
					} else if j+1 < len(text) && text[j+1] == '"' {
						j += 2
					} else {
						j++
						break
					}
				}
				i = j
			default:
				j := i + 1
				for j < len(text) && mIdentCont(text[j]) {
					j++
				}
				i = j
			}
		default:
			i++
		}
	}
	return spans, state
}

// --- scan.go's side ---

func probeSpans(text string, backslashEscapes bool) []mspan {
	var spans []mspan
	for i := 0; i < len(text); {
		kind, end, _ := nextSQLToken(text, i, backslashEscapes)
		switch kind {
		case sqlString, sqlDollarString:
			spans = append(spans, mspan{start: i, end: end})
		case sqlUnicodeEscape:
			if i+2 < len(text) && text[i+2] == '"' {
				break // U&"name" is a name under both readings
			}
			spans = append(spans, mspan{start: i, end: end})
		case sqlComment:
			spans = append(spans, mspan{start: i, end: end, comment: true})
		}
		i = end
	}
	return spans
}

func covered(spans []mspan, from, to int) bool {
	for _, s := range spans {
		if s.start < to && from < s.end {
			return true
		}
	}
	return false
}

func hiddenBytes(spans []mspan, n int) []bool {
	out := make([]bool, n)
	for _, s := range spans {
		for i := s.start; i < s.end && i < n; i++ {
			out[i] = true
		}
	}
	return out
}

var alphabet = []string{
	"'", "''", `\`, `\'`, `"`, "--", "/*", "*/", "\n", " ", "$t$", "$", "1", ",",
	"B'", "X'", "N'", "E'", "U&'", "a",
}

const marker = "zqz"

// eachText calls visit with every text of up to maxLen pieces of the alphabet, and with the
// marker word inserted at every gap of each when withMarker. It streams rather than collecting,
// so maxLen 5 with the marker (19.2 million texts) fits in memory.
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

// TestDifferentialSpansAgainstScanL compares, text by text, the spans scan.go's two readings hold
// against the model above, and checks the invariant that matters: no byte inside a literal or a
// comment of either reading is printed.
func TestDifferentialSpansAgainstScanL(t *testing.T) {
	var unsafeSpan, overSpan, junk int
	unsafeExamples := map[string]bool{}
	overExamples := map[string]bool{}
	n := eachText(5, false, func(text string) {
		for _, scsOn := range []bool{true, false} {
			model, state := modelSpans(text, scsOn)
			if state == mJunk {
				junk++
				continue
			}
			mine := probeSpans(text, !scsOn)
			m := hiddenBytes(model, len(text))
			s := hiddenBytes(mine, len(text))
			for i := range m {
				if m[i] && !s[i] {
					unsafeSpan++
					if len(unsafeExamples) < 12 {
						unsafeExamples[fmt.Sprintf("scs_on=%v %q byte %d", scsOn, text, i)] = true
					}
					break
				}
			}
			for i := range m {
				if s[i] && !m[i] {
					overSpan++
					if len(overExamples) < 12 {
						overExamples[fmt.Sprintf("scs_on=%v %q byte %d", scsOn, text, i)] = true
					}
					break
				}
			}
		}
	})
	t.Logf("span corpus: %d texts, %d (text,setting) pairs skipped as numeric junk", n, junk)
	t.Logf("texts where the model hides a byte scan.go does not (unsafe): %d", unsafeSpan)
	for e := range unsafeExamples {
		t.Logf("  unsafe: %s", e)
	}
	t.Logf("texts where scan.go hides a byte the model does not (over-hiding, safe): %d", overSpan)
	for e := range overExamples {
		t.Logf("  over: %s", e)
	}
	if unsafeSpan > 0 {
		t.Errorf("%d texts where the model hides a byte scan.go does not", unsafeSpan)
	}
}

// TestDifferentialMarkerIsNeverPrinted is the invariant itself: a marker word inside a literal or
// a comment of either real reading must not survive redactedQuery.
func TestDifferentialMarkerIsNeverPrinted(t *testing.T) {
	var checked, leaked, junk int
	examples := map[string]bool{}
	n := eachText(5, true, func(text string) {
		at := strings.Index(text, marker)
		hiddenOn, stateOn := modelSpans(text, true)
		hiddenOff, stateOff := modelSpans(text, false)
		if stateOn == mJunk || stateOff == mJunk {
			junk++
			return
		}
		hidden := covered(hiddenOn, at, at+len(marker)) || covered(hiddenOff, at, at+len(marker))
		if !hidden {
			return
		}
		checked++
		if strings.Contains(redactedQuery(text), marker) {
			leaked++
			if len(examples) < 20 {
				examples[fmt.Sprintf("%q -> %q", text, redactedQuery(text))] = true
			}
		}
	})
	t.Logf("marker corpus: %d texts, %d skipped as numeric junk, %d with the marker inside a literal or comment of either reading", n, junk, checked)
	t.Logf("leaks: %d", leaked)
	for e := range examples {
		t.Logf("  leak: %s", e)
	}
	if leaked > 0 {
		t.Errorf("%d leaks", leaked)
	}
}

// TestDifferentialModelAgainstServer checks the model one-directionally against a real server: a
// marker the model holds inside a literal or a comment must not be a token the server names.
func TestDifferentialModelAgainstServer(t *testing.T) {
	url := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set")
	}
	ctx := context.Background()
	// Every 29th text of the four-piece corpus, so the sample spreads over the whole of it.
	var sample []string
	i := 0
	eachText(4, true, func(text string) {
		if i%29 == 0 {
			sample = append(sample, "select "+text)
		}
		i++
	})
	var asked, disagreed int
	examples := map[string]bool{}
	for _, scs := range []string{"on", "off"} {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if _, err := conn.Exec(ctx, "set standard_conforming_strings = "+scs); err != nil {
			t.Fatalf("set: %v", err)
		}
		for _, text := range sample {
			model, state := modelSpans(text, scs == "on")
			if state == mJunk {
				continue
			}
			at := strings.Index(text, marker)
			if !covered(model, at, at+len(marker)) {
				continue // the model says it is structure; the server cannot refute that
			}
			asked++
			_, err := conn.Exec(ctx, text)
			if err == nil {
				continue
			}
			message := err.Error()
			if strings.Contains(message, `column "`+marker+`" does not exist`) ||
				strings.Contains(message, `at or near "`+marker+`"`) {
				disagreed++
				if len(examples) < 20 {
					examples[fmt.Sprintf("scs=%s %q: %s", scs, text, message)] = true
				}
			}
		}
		conn.Close(ctx)
	}
	t.Logf("server check: %d statements the model holds the marker hidden in, %d the server names the marker as a token of its own", asked, disagreed)
	for e := range examples {
		t.Logf("  model disagreement: %s", e)
	}
	if disagreed > 0 {
		t.Errorf("%d model disagreements", disagreed)
	}
}
