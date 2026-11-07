package pgmigrate

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file is a transcription of PostgreSQL 18.6's src/backend/parser/scan.l: its start states,
// its definitions and its rules, in the file's own order, each named beside the Go that stands
// for it so the two can be read side by side. It is a transcription and not a reading of the
// grammar: where scan.l has a state, lexSQL has that state; where scan.l has a rule, lexRules has
// a rule of the same name; where scan.l's action does yyless, BEGIN or yyerror, the Go action
// does the same thing. Nothing here decides what Postgres does - every such decision is scan.l's,
// quoted above the code that carries it.
//
// The one thing flex does for scan.l that the Go must do for itself is choosing between rules:
// the longest match at the scan position wins, and the earliest rule in the file wins a tie
// (flex(1), "How the input is matched"). lexSQL's driver does exactly that over the rules of the
// state it is in, which is why the rules are kept in scan.l's order and why a rule may not be
// moved without checking the ties it would change. INITIAL holds 41 of scan.l's 41 INITIAL
// rules, in scan.l's order, and every other start state all of its own, but for <xqs>, whose
// three rules - {quotecontinuefail}, {other} and <<EOF>> - share one action and are folded into
// the driver's branch for that state.
//
// scan.l raises errors (yyerror, ereport(ERROR)) for text Postgres refuses: an unterminated
// literal or comment, trailing junk after a number or a parameter, an invalid integer, a bad
// Unicode escape, a parameter number no int32 holds, an operator of NAMEDATALEN bytes or more,
// an escape that makes a byte the server encoding cannot hold, and U&'…' with
// standard_conforming_strings off. A session that ran such a statement inside a transaction sits
// "idle in transaction (aborted)" holding every lock it took before, and pg_stat_activity
// records the refused text, so the redactor is shown exactly these texts and may not treat them
// as unreachable. lexSQL therefore reports where scan.l first raised an error, and goes on
// reading afterwards so that the readers which only want tokens (scanSQL) are not cut short;
// what redactedQuery does from that point is its own rule, documented there. A refusal left out
// of this file is a tail of a refused statement printed, which is why each of them is written
// where scan.l writes it.
//
// Two things this reading does not take, and one it takes at a single value. It is the scan.l of
// one major version (REL_18_STABLE, read against the 18.6 this repository tests on), and it
// takes standard_conforming_strings and no other setting - backslash_quote above all, whose off
// turns <xe>{xeescape}'s \' into "unsafe use of \' in a string literal", measured on 18.6:
// select e'\'' , 1 answers that error with backslash_quote off and answers ' | 1 with it at its
// default. The server encoding it takes as UTF-8, which addunicode needs to turn a \uXXXX into
// bytes and pg_verifymbstr needs to say whether an escape made a byte that encoding cannot hold
// (verifymbstr): UTF-8 is the server_encoding of the 18.6 this module is read against, and
// against a single-byte server this reading refuses a high byte that server reads, which covers
// more of a text rather than less. Neither of the two moves where a literal or a comment begins
// or ends: each only means Postgres refused a text this reading reads on through, so the rule
// redactedQuery holds - a byte is printed only where both readings read it as structure - stands
// as it is, and what is missed is the extra conservatism of hiding the refused tail. Reading the
// holder's backslash_quote the way the two standard_conforming_strings are read, as a third
// reading, is a question for whoever owns this rule rather than something to settle here.
//
// libpg_query (pg_query_go) would close neither. Its scanner is one major version's too, it is
// compiled with standard_conforming_strings on and offers no way to turn it off, so the other
// reading would still be written by hand, and it offers no way to set backslash_quote or the
// server encoding either; against that it costs this module a cgo dependency and a build that is
// no longer pure Go. What it would give is that a version bump tracks scan.l for free, which is
// what this file asks a reader to do by diffing it.

// lexState is one of scan.l's start states (its %x declarations), INITIAL included.
type lexState int

const (
	lexInitial lexState = iota
	lexXB               // <xb> bit string
	lexXC               // <xc> extended C-style comment
	lexXD               // <xd> delimited identifier
	lexXDolq            // <xdolq> dollar-quoted string
	lexXE               // <xe> extended quoted string with backslash escapes
	lexXEU              // <xeu> Unicode surrogate pair in extended quoted string
	lexXH               // <xh> hexadecimal byte string
	lexXQ               // <xq> standard quoted string
	lexXQS              // <xqs> quote stop: look ahead for a quote continuation
	lexXUI              // <xui> quoted identifier with Unicode escapes
	lexXUS              // <xus> quoted string with Unicode escapes
	lexStates
)

// lexer holds what scan.l keeps in yyscanner and yyextra for one reading of one text.
type lexer struct {
	text string
	// standardConformingStrings is yyextra->standard_conforming_strings: the reading session's
	// setting, which a query text does not carry.
	standardConformingStrings bool

	state  lexState
	pos    int // the scan position, flex's yy_c_buf_p
	start  int // yylloc, which SET_YYLLOC() sets to the start of the token being read
	kind   sqlTokenKind
	before lexState        // yyextra->state_before_str_stop
	depth  int             // yyextra->xcdepth
	dolq   string          // yyextra->dolqstart
	lit    strings.Builder // yyextra->literalbuf, emptied by startlit() and filled by addlit()
	emit   func(sqlToken)

	// sawNonASCII is yyextra->saw_non_ascii: an escape of <xe> has made a byte that is not
	// 7-bit ASCII, which is what makes the <xqs> action verify the literal it assembled.
	sawNonASCII bool

	// utf16First is yyextra->utf16_first_part, the first half of the surrogate pair <xeu> is
	// reading the second half of.
	utf16First int

	// op is the run of {op_chars} last measured, which every token read inside it is answered
	// from (opRun).
	op opRun

	// failAt is where scan.l first raised an error, or -1. Most error rules report
	// lexer_errposition(), which is the yylloc SET_YYLLOC() last set, so failAt is the first byte
	// of the token the error rule read rather than the byte inside it the rule stopped on. Two
	// exceptions. The escape rules of <xe> and <xeu> PUSH_YYLLOC() and SET_YYLLOC() to point the
	// cursor at the escape sequence itself, so Postgres reports a byte inside the literal where
	// this reading reports the literal's start. And pg_verifymbstr, which the <xqs> action calls
	// over the literal it assembled, reports no position at all: it is raised in mbutils.c
	// outside the scanner, so PostgreSQL 18.6 answers select e'\400' with "invalid byte sequence
	// for encoding "UTF8": 0x00" and no LINE or caret, where this reading reports the literal's
	// start. Both go the same way - the report covers from the literal's start, which is at or
	// before any byte Postgres named - and that is why a reading of this field may not be called
	// a reading of lexer_errposition().
	failAt int
}

// lexRule is one rule of scan.l's rules section: the name of its pattern as scan.l spells it, the
// pattern as a matcher returning the length of its match at i (-1 for no match, which a
// zero-length match is also reported as, flex never choosing one here), and its action.
type lexRule struct {
	name  string
	match func(l *lexer, i int) int
	act   func(l *lexer, i, n int)
}

// lexRules holds, per start state, the rules of scan.l that are active in it, in scan.l's order.
var lexRules = [lexStates][]lexRule{
	lexInitial: {
		{"{whitespace}", matchWhitespace, actWhitespace},
		{"{xcstart}", matchXcstart, actXcstart},
		{"{xbstart}", matchXbstart, actXbstart},
		{"{xhstart}", matchXhstart, actXhstart},
		{"{xnstart}", matchXnstart, actXnstart},
		{"{xqstart}", matchQuote, actXqstart},
		{"{xestart}", matchXestart, actXestart},
		{"{xusstart}", matchXusstart, actXusstart},
		{"{dolqdelim}", matchDolqdelim, actDolqdelim},
		{"{dolqfailed}", matchDolqfailed, actDolqfailed},
		{"{xdstart}", matchDquote, actXdstart},
		{"{xuistart}", matchXuistart, actXuistart},
		{"{xufailed}", matchXufailed, actXufailed},
		{"{typecast}", literal("::"), actOneToken(sqlOther)},
		{"{dot_dot}", literal(".."), actOneToken(sqlOther)},
		{"{colon_equals}", literal(":="), actOneToken(sqlOther)},
		{"{equals_greater}", literal("=>"), actOneToken(sqlOther)},
		{"{less_equals}", literal("<="), actOneToken(sqlOther)},
		{"{greater_equals}", literal(">="), actOneToken(sqlOther)},
		{"{less_greater}", literal("<>"), actOneToken(sqlOther)},
		{"{not_equals}", literal("!="), actOneToken(sqlOther)},
		{"{self}", matchSelf, actOneToken(sqlOther)},
		{"{operator}", matchOperator, actOperator},
		{"{param}", matchParam, actParam},
		{"{param_junk}", matchParamJunk, actJunk(sqlParam)},
		{"{decinteger}", matchDecinteger, actOneToken(sqlNumber)},
		{"{hexinteger}", matchHexinteger, actOneToken(sqlNumber)},
		{"{octinteger}", matchOctinteger, actOneToken(sqlNumber)},
		{"{bininteger}", matchBininteger, actOneToken(sqlNumber)},
		{"{hexfail}", matchHexfail, actJunk(sqlNumber)},
		{"{octfail}", matchOctfail, actJunk(sqlNumber)},
		{"{binfail}", matchBinfail, actJunk(sqlNumber)},
		{"{numeric}", matchNumeric, actOneToken(sqlNumber)},
		{"{numericfail}", matchNumericfail, actNumericfail},
		{"{real}", matchReal, actOneToken(sqlNumber)},
		{"{realfail}", matchRealfail, actJunk(sqlNumber)},
		{"{integer_junk}", matchIntegerJunk, actJunk(sqlNumber)},
		{"{numeric_junk}", matchNumericJunk, actJunk(sqlNumber)},
		{"{real_junk}", matchRealJunk, actJunk(sqlNumber)},
		{"{identifier}", matchIdentifier, actOneToken(sqlWord)},
		{"{other}", matchOther, actOneToken(sqlOther)},
	},
	lexXC: {
		{"<xc>{xcstart}", matchXcstart, actXcDeeper},
		{"<xc>{xcstop}", matchXcstop, actXcstop},
		{"<xc>{xcinside}", matchXcinside, actSkip},
		{"<xc>{op_chars}", matchOpChar, actSkip},
		{`<xc>\*+`, matchStars, actSkip},
	},
	lexXB: {
		{"<xb>{xbinside}", matchXbinside, actSkip},
		{"<xb>{quote}", matchQuote, actQuoteStop},
	},
	lexXH: {
		{"<xh>{xhinside}", matchXhinside, actSkip},
		{"<xh>{quote}", matchQuote, actQuoteStop},
	},
	lexXQ: {
		{"<xq>{quote}", matchQuote, actQuoteStop},
		{"<xq>{xqdouble}", literal("''"), actSkip},
		{"<xq>{xqinside}", matchXqinside, actSkip},
	},
	lexXE: {
		{"<xe>{quote}", matchQuote, actQuoteStop},
		{"<xe>{xqdouble}", literal("''"), actXqdouble},
		{"<xe>{xeinside}", matchXeinside, actAddLit},
		{"<xe>{xeunicode}", matchXeunicode, actXeunicode},
		{"<xe>{xeunicodefail}", matchXeunicodefail, actXeunicodefail},
		{"<xe>{xeescape}", matchXeescape, actXeescape},
		{"<xe>{xeoctesc}", matchXeoctesc, actXeoctesc},
		{"<xe>{xehexesc}", matchXehexesc, actXehexesc},
		{"<xe>.", matchDot, actAddLit},
	},
	lexXEU: {
		{"<xeu>{xeunicode}", matchXeunicode, actXeuSecond},
		{"<xeu>.", matchDot, actXeuFail},
		{`<xeu>\n`, matchNewlineByte, actXeuFail},
		{"<xeu>{xeunicodefail}", matchXeunicodefail, actXeunicodefail},
	},
	lexXUS: {
		{"<xus>{quote}", matchQuote, actQuoteStop},
		{"<xus>{xqdouble}", literal("''"), actSkip},
		{"<xus>{xqinside}", matchXqinside, actSkip},
	},
	lexXDolq: {
		{"<xdolq>{dolqdelim}", matchDolqdelim, actXdolqDelim},
		{"<xdolq>{dolqinside}", matchDolqinside, actSkip},
		{"<xdolq>{dolqfailed}", matchDolqfailed, actSkip},
		{"<xdolq>.", matchDot, actSkip},
	},
	lexXD: {
		{"<xd>{xdstop}", matchDquote, actXdstop},
		{"<xd>{xddouble}", literal(`""`), actXddouble},
		{"<xd>{xdinside}", matchXdinside, actXdinside},
	},
	lexXUI: {
		{"<xui>{dquote}", matchDquote, actXdstop},
		{"<xui>{xddouble}", literal(`""`), actXddouble},
		{"<xui>{xdinside}", matchXdinside, actXdinside},
	},
}

// lexSQL reads text as scan.l reads it with standard_conforming_strings standardConformingStrings,
// calling emit with every token in order, and returns where scan.l first raised an error, or -1.
func lexSQL(text string, standardConformingStrings bool, emit func(sqlToken)) int {
	l := &lexer{text: text, standardConformingStrings: standardConformingStrings, emit: emit, failAt: -1}
	for l.pos < len(text) {
		if l.state == lexXQS {
			// <xqs>{quotecontinue} is the only rule of this state with an action of its own.
			// <xqs>{quotecontinuefail}, <xqs>{other} and <xqs><<EOF>> share one action: throw
			// everything after the end quote back with yyless(0) and return the literal. That
			// action runs wherever {quotecontinue} does not match, so the three need no
			// patterns of their own and lexRules holds no row for this state.
			if n := matchQuotecontinue(l, l.pos); n > 0 {
				actQuotecontinue(l, l.pos, n)
			} else {
				actQuoteStopFail(l, l.pos, 0)
			}
			continue
		}
		best, length := -1, 0
		for r := range lexRules[l.state] {
			// flex takes the longest match, and the earliest rule of the file in a tie.
			if n := lexRules[l.state][r].match(l, l.pos); n > length {
				best, length = r, n
			}
		}
		if best < 0 {
			// No rule of this state matches. Every state of scan.l has a rule for any one byte,
			// so this is a state of this reading that has lost one, and the byte it cannot read
			// is a byte whose meaning it does not know: a carriage return in <xeu> was exactly
			// that, and skipping it printed the tail of a refused statement. Failing here covers
			// the rest of the text, which is the answer a reading that cannot go on owes, and
			// taking the byte keeps the loop finite.
			l.fail()
			l.pos++
			continue
		}
		lexRules[l.state][best].act(l, l.pos, length)
	}
	l.atEOF()
	return l.failAt
}

// atEOF runs scan.l's <<EOF>> rules, which are named beside each case.
func (l *lexer) atEOF() {
	switch l.state {
	case lexInitial:
		// <<EOF>>: yyterminate().
	case lexXQS:
		// <xqs><<EOF>>: yyless(0) and return the literal the lookahead began from.
		actQuoteStopFail(l, l.pos, 0)
	case lexXC:
		// <xc><<EOF>>: yyerror("unterminated /* comment").
		l.fail()
		l.ret(sqlComment, len(l.text))
	case lexXB:
		// <xb><<EOF>>: yyerror("unterminated bit string literal").
		l.fail()
		l.ret(sqlString, len(l.text))
	case lexXH:
		// <xh><<EOF>>: yyerror("unterminated hexadecimal string literal").
		l.fail()
		l.ret(sqlString, len(l.text))
	case lexXQ, lexXE, lexXUS:
		// <xq,xe,xus><<EOF>>: yyerror("unterminated quoted string").
		l.fail()
		l.ret(l.kind, len(l.text))
	case lexXEU:
		// <xeu><<EOF>>: yyerror("invalid Unicode surrogate pair").
		l.fail()
		l.ret(l.kind, len(l.text))
	case lexXDolq:
		// <xdolq><<EOF>>: yyerror("unterminated dollar-quoted string").
		l.fail()
		l.ret(sqlDollarString, len(l.text))
	case lexXD, lexXUI:
		// <xd,xui><<EOF>>: yyerror("unterminated quoted identifier").
		l.fail()
		l.ret(l.kind, len(l.text))
	}
}

// fail records where scan.l raised its first error: the start of the token being read, which is
// the yylloc its SET_YYLLOC() set and so the position lexer_errposition() reports.
func (l *lexer) fail() {
	if l.failAt < 0 {
		l.failAt = l.start
	}
}

// ret is scan.l's "return <token>": the token runs from the yylloc SET_YYLLOC() set to end, the
// scanner goes back to INITIAL, and scanning carries on at end.
func (l *lexer) ret(kind sqlTokenKind, end int) {
	token := sqlToken{kind: kind, start: l.start, end: end}
	if kind == sqlQuotedIdentifier {
		token.identifier = l.lit.String()
	}
	l.emit(token)
	l.state = lexInitial
	l.pos = end
}

// --- the matchers, one per definition of scan.l's definitions section ---

// space [ \t\n\r\f\v]
func isLexSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// non_newline_space [ \t\f\v]
func isLexNonNewlineSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\f' || c == '\v'
}

// newline [\n\r]
func isLexNewline(c byte) bool { return c == '\n' || c == '\r' }

// comment ("--"{non_newline}*)
func matchComment(text string, i int) int {
	if !strings.HasPrefix(text[i:], "--") {
		return -1
	}
	j := i + 2
	for j < len(text) && !isLexNewline(text[j]) {
		j++
	}
	return j - i
}

// whitespace ({space}+|{comment})
func matchWhitespace(l *lexer, i int) int {
	if isLexSpace(l.text[i]) {
		j := i
		for j < len(l.text) && isLexSpace(l.text[j]) {
			j++
		}
		return j - i
	}
	return matchComment(l.text, i)
}

// whitespace_with_newline ({non_newline_whitespace}*{newline}{special_whitespace}*), with
// non_newline_whitespace ({non_newline_space}|{comment}) and special_whitespace
// ({space}+|{comment}{newline}). The alternatives of each star begin with different bytes - a
// space, or the dash of a comment - and a comment runs to the newline that ends it, so taking
// each as far as it goes matches what flex matches; there is nothing to back out of.
func matchWhitespaceWithNewline(text string, i int) int {
	j := i
	for j < len(text) { // {non_newline_whitespace}*
		if isLexNonNewlineSpace(text[j]) {
			j++
		} else if n := matchComment(text, j); n > 0 {
			j += n
		} else {
			break
		}
	}
	if j >= len(text) || !isLexNewline(text[j]) { // {newline}
		return -1
	}
	j++
	for j < len(text) { // {special_whitespace}*
		if isLexSpace(text[j]) {
			j++
		} else if n := matchComment(text, j); n > 0 && j+n < len(text) && isLexNewline(text[j+n]) {
			j += n + 1 // {comment}{newline}: the comment counts only with its newline
		} else {
			break
		}
	}
	return j - i
}

// quote '
func matchQuote(l *lexer, i int) int {
	if l.text[i] == '\'' {
		return 1
	}
	return -1
}

// quotecontinue {whitespace_with_newline}{quote}
func matchQuotecontinue(l *lexer, i int) int {
	n := matchWhitespaceWithNewline(l.text, i)
	if n < 0 || i+n >= len(l.text) || l.text[i+n] != '\'' {
		return -1
	}
	return n + 1
}

// xbstart [bB]{quote}
func matchXbstart(l *lexer, i int) int { return matchPrefixQuote(l.text, i, "bB", 1) }

// xhstart [xX]{quote}
func matchXhstart(l *lexer, i int) int { return matchPrefixQuote(l.text, i, "xX", 1) }

// xnstart [nN]{quote}
func matchXnstart(l *lexer, i int) int { return matchPrefixQuote(l.text, i, "nN", 1) }

// xestart [eE]{quote}
func matchXestart(l *lexer, i int) int { return matchPrefixQuote(l.text, i, "eE", 1) }

// xusstart [uU]&{quote}
func matchXusstart(l *lexer, i int) int { return matchPrefixQuote(l.text, i, "uU", 2) }

// xuistart [uU]&{dquote}
func matchXuistart(l *lexer, i int) int {
	if i+2 < len(l.text) && (l.text[i] == 'u' || l.text[i] == 'U') && l.text[i+1] == '&' && l.text[i+2] == '"' {
		return 3
	}
	return -1
}

// xufailed [uU]&
func matchXufailed(l *lexer, i int) int {
	if i+1 < len(l.text) && (l.text[i] == 'u' || l.text[i] == 'U') && l.text[i+1] == '&' {
		return 2
	}
	return -1
}

// matchPrefixQuote matches one of starts, then (for the U& forms) an ampersand, then a quote.
func matchPrefixQuote(text string, i int, starts string, width int) int {
	if strings.IndexByte(starts, text[i]) < 0 {
		return -1
	}
	if width == 2 && (i+1 >= len(text) || text[i+1] != '&') {
		return -1
	}
	if i+width >= len(text) || text[i+width] != '\'' {
		return -1
	}
	return width + 1
}

// xbinside [^']*, xhinside [^']*: flex never takes the empty match, {quote} or <<EOF>> applying
// where it would, so a run of no bytes is reported as no match.
func matchXbinside(l *lexer, i int) int { return runUntilByte(l.text, i, '\'') }
func matchXhinside(l *lexer, i int) int { return runUntilByte(l.text, i, '\'') }

// xqinside [^']+
func matchXqinside(l *lexer, i int) int { return runUntilByte(l.text, i, '\'') }

// xeinside [^\\']+
func matchXeinside(l *lexer, i int) int {
	j := i
	for j < len(l.text) && l.text[j] != '\\' && l.text[j] != '\'' {
		j++
	}
	if j == i {
		return -1
	}
	return j - i
}

// xeescape [\\][^0-7]
func matchXeescape(l *lexer, i int) int {
	if l.text[i] == '\\' && i+1 < len(l.text) && (l.text[i+1] < '0' || l.text[i+1] > '7') {
		return 2
	}
	return -1
}

// xeoctesc [\\][0-7]{1,3}
func matchXeoctesc(l *lexer, i int) int {
	if l.text[i] != '\\' {
		return -1
	}
	j := i + 1
	for j < len(l.text) && j < i+4 && '0' <= l.text[j] && l.text[j] <= '7' {
		j++
	}
	if j == i+1 {
		return -1
	}
	return j - i
}

// xehexesc [\\]x[0-9A-Fa-f]{1,2}
func matchXehexesc(l *lexer, i int) int {
	if l.text[i] != '\\' || i+1 >= len(l.text) || l.text[i+1] != 'x' {
		return -1
	}
	j := i + 2
	for j < len(l.text) && j < i+4 && isHexDigit(l.text[j]) {
		j++
	}
	if j == i+2 {
		return -1
	}
	return j - i
}

// xeunicode [\\](u[0-9A-Fa-f]{4}|U[0-9A-Fa-f]{8})
func matchXeunicode(l *lexer, i int) int {
	if l.text[i] != '\\' || i+1 >= len(l.text) {
		return -1
	}
	want := 0
	switch l.text[i+1] {
	case 'u':
		want = 4
	case 'U':
		want = 8
	default:
		return -1
	}
	if i+2+want > len(l.text) {
		return -1
	}
	for j := i + 2; j < i+2+want; j++ {
		if !isHexDigit(l.text[j]) {
			return -1
		}
	}
	return 2 + want
}

// xeunicodefail [\\](u[0-9A-Fa-f]{0,3}|U[0-9A-Fa-f]{0,7})
func matchXeunicodefail(l *lexer, i int) int {
	if l.text[i] != '\\' || i+1 >= len(l.text) {
		return -1
	}
	most := 0
	switch l.text[i+1] {
	case 'u':
		most = 3
	case 'U':
		most = 7
	default:
		return -1
	}
	j := i + 2
	for j < len(l.text) && j < i+2+most && isHexDigit(l.text[j]) {
		j++
	}
	return j - i
}

// dolq_start [A-Za-z\200-\377_], dolq_cont [A-Za-z\200-\377_0-9]
func isDolqStart(c byte) bool { return isIdentStart(c) }
func isDolqCont(c byte) bool  { return isIdentStart(c) || isDigit(c) }

// dolqdelim \$({dolq_start}{dolq_cont}*)?\$
func matchDolqdelim(l *lexer, i int) int {
	if l.text[i] != '$' {
		return -1
	}
	j := i + 1
	if j < len(l.text) && isDolqStart(l.text[j]) {
		for j++; j < len(l.text) && isDolqCont(l.text[j]); j++ {
		}
	}
	if j >= len(l.text) || l.text[j] != '$' {
		return -1
	}
	return j + 1 - i
}

// dolqfailed \${dolq_start}{dolq_cont}*
func matchDolqfailed(l *lexer, i int) int {
	if l.text[i] != '$' || i+1 >= len(l.text) || !isDolqStart(l.text[i+1]) {
		return -1
	}
	j := i + 2
	for j < len(l.text) && isDolqCont(l.text[j]) {
		j++
	}
	return j - i
}

// dolqinside [^$]+
func matchDolqinside(l *lexer, i int) int { return runUntilByte(l.text, i, '$') }

// dquote \"
func matchDquote(l *lexer, i int) int {
	if l.text[i] == '"' {
		return 1
	}
	return -1
}

// xdinside [^"]+
func matchXdinside(l *lexer, i int) int { return runUntilByte(l.text, i, '"') }

// xcstart \/\*{op_chars}*
func matchXcstart(l *lexer, i int) int {
	if !strings.HasPrefix(l.text[i:], "/*") {
		return -1
	}
	return l.opRunTo(i+2) - i
}

// xcstop \*+\/
func matchXcstop(l *lexer, i int) int {
	if l.text[i] != '*' {
		return -1
	}
	j := i
	for j < len(l.text) && l.text[j] == '*' {
		j++
	}
	if j >= len(l.text) || l.text[j] != '/' {
		return -1
	}
	return j + 1 - i
}

// xcinside [^*/]+
func matchXcinside(l *lexer, i int) int {
	j := i
	for j < len(l.text) && l.text[j] != '*' && l.text[j] != '/' {
		j++
	}
	if j == i {
		return -1
	}
	return j - i
}

// \*+ , the <xc> rule that takes stars no {xcstop} follows
func matchStars(l *lexer, i int) int {
	j := i
	for j < len(l.text) && l.text[j] == '*' {
		j++
	}
	if j == i {
		return -1
	}
	return j - i
}

// ident_start [A-Za-z\200-\377_] and ident_cont [A-Za-z\200-\377_0-9\$] are isIdentStart and
// isIdentCont in scan.go, which the three readers of scanSQL share.

// identifier {ident_start}{ident_cont}*. The driver only ever asks a matcher about a byte there
// is, but {integer_junk} and its kin ask this one about the byte after a number, which can be
// the end of the text.
func matchIdentifier(l *lexer, i int) int {
	if i >= len(l.text) || !isIdentStart(l.text[i]) {
		return -1
	}
	j := i + 1
	for j < len(l.text) && isIdentCont(l.text[j]) {
		j++
	}
	return j - i
}

// self [,()\[\].;\:\+\-\*\/\%\^\<\>\=]
func isSelfChar(c byte) bool { return strings.IndexByte(`,()[].;:+-*/%^<>=`, c) >= 0 }

func matchSelf(l *lexer, i int) int {
	if isSelfChar(l.text[i]) {
		return 1
	}
	return -1
}

// opCharRead, where a test has set it, is called once for every byte this reading asks isOpChar
// about, and that is what it counts. What such a count reaches and what it misses is stated where
// it is read, in TestOperatorRunLexingIsLinear's header, rather than here. In every process but a
// test of this package the hook is nil, and the cost is the comparison below.
var opCharRead func()

// op_chars [\~\!\@\#\^\&\|\`\?\+\-\*\/\%\<\>\=]
func isOpChar(c byte) bool {
	if opCharRead != nil {
		opCharRead()
	}
	return strings.IndexByte("~!@#^&|`?+-*/%<>=", c) >= 0
}

func matchOpChar(l *lexer, i int) int {
	if isOpChar(l.text[i]) {
		return 1
	}
	return -1
}

// opRun is one maximal run of {op_chars} read from one byte of it on, and the three things
// scan.l's {operator} action asks about a token that begins there. Two of the three are the
// run's rather than the token's - the action stops the token where the run's first "/*" or "--"
// begins, and looks back from that same end for a qualifying character and for the trailing run
// of [+-] - so a run read once answers for every token read in it up to that stop.
//
// That is what keeps this reading's cost the text's length rather than its square. Flex asks
// {operator} and {xcstart} about every byte of such a run while their actions take one or two
// bytes of it at a time (yyless(nchars), yyless(2)), so measuring the run on every byte measures
// it once per byte of itself, and 64 KiB of +- read between 524,294 and 655,366 times its own
// length that way against the 10.00 times it reads this way (TestOperatorRunLexingIsLinear). This reader is
// handed what pg_stat_activity holds, which track_activity_query_size bounds at 1 MB.
type opRun struct {
	from, to int // the run itself, [from, to)
	stop     int // where the first "/*" or "--" at or after from begins, or to where there is none
	qualify  int // the last byte of "~!@#^&|`?%" before stop-1, or -1 where there is none
	plus     int // where the run of [+-] that ends at stop begins, or stop where there is none
}

// opRunTo is the end of the run of {op_chars} that holds i, which is i itself where i is no op
// char. A run is maximal, so the run last measured ends where the run of every position inside
// it ends.
func (l *lexer) opRunTo(i int) int {
	if i >= l.op.from && i < l.op.to {
		return l.op.to
	}
	l.op = l.measureOpRun(i)
	return l.op.to
}

// opRunAt is the run read from i on. The run last measured answers for every position inside it
// that its stop has not passed; at or past that stop a comment has opened, and what the scanner
// reads when it comes back into the run after that comment is a run read from there on.
func (l *lexer) opRunAt(i int) opRun {
	if i >= l.op.from && i < l.op.stop {
		return l.op
	}
	l.op = l.measureOpRun(i)
	return l.op
}

// opRunScanned, where a test has set it, is told what each measureOpRun read of the text beyond
// its scan of {op_chars}: the run it measured, and where each of its two searches stopped (-1
// where the search found nothing and so read to its end). The scan is counted by opCharRead
// above rather than reported from here, because a count this function kept of its own extent
// loop would be a claim about which of its branches ran; what that count does and does not
// reach is stated where it is read, in TestOperatorRunLexingIsLinear's header. The test sets
// both hooks and adds the two together; in every process but a test of this package each is nil
// and the call is one comparison.
var opRunScanned func(run opRun, slash, dash int)

// measureOpRun reads the run from i on. Each of its three searches stops at what it is looking
// for, and the run's end is taken from the run last measured where that one holds i, so reading
// a run again after a comment inside it costs the bytes up to the next comment rather than the
// bytes of the whole run. That reuse is the branch a text of comments inside one operator run
// takes, and the case of TestOperatorRunLexingIsLinear that exercises it is "+/**/": with the
// branch taken out, every other test of this package stays green while 64 KiB of that text reads
// 13,110.4 times its own length, which is what that case measures.
func (l *lexer) measureOpRun(i int) opRun {
	run := opRun{from: i, to: i, qualify: -1}
	if i >= l.op.from && i < l.op.to {
		run.to = l.op.to
	} else {
		for run.to < len(l.text) && isOpChar(l.text[run.to]) {
			run.to++
		}
	}
	run.stop = run.to
	slash := strings.Index(l.text[i:run.to], "/*")
	if slash >= 0 {
		run.stop = i + slash
	}
	dash := strings.Index(l.text[i:run.stop], "--")
	if dash >= 0 {
		run.stop = i + dash
	}
	run.plus = run.stop
	for run.plus > i && (l.text[run.plus-1] == '+' || l.text[run.plus-1] == '-') {
		run.plus--
	}
	for at := run.stop - 2; at >= i; at-- {
		if strings.IndexByte("~!@#^&|`?%", l.text[at]) >= 0 {
			run.qualify = at
			break
		}
	}
	if opRunScanned != nil {
		opRunScanned(run, slash, dash)
	}
	return run
}

// operator {op_chars}+
func matchOperator(l *lexer, i int) int {
	if to := l.opRunTo(i); to > i {
		return to - i
	}
	return -1
}

// decdigit [0-9] is isDigit in scan.go; hexdigit [0-9A-Fa-f], octdigit [0-7], bindigit [0-1].
func isHexDigit(c byte) bool {
	return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
func isOctDigit(c byte) bool { return '0' <= c && c <= '7' }
func isBinDigit(c byte) bool { return c == '0' || c == '1' }

// decinteger {decdigit}(_?{decdigit})*
func matchDecinteger(l *lexer, i int) int { return matchDigits(l.text, i, isDigit) }

// matchDigits reads {digit}(_?{digit})*, the shape all four integer rules share.
func matchDigits(text string, i int, digit func(byte) bool) int {
	if i >= len(text) || !digit(text[i]) {
		return -1
	}
	j := i + 1
	for j < len(text) {
		if digit(text[j]) {
			j++
		} else if text[j] == '_' && j+1 < len(text) && digit(text[j+1]) {
			j += 2
		} else {
			break
		}
	}
	return j - i
}

// hexinteger 0[xX](_?{hexdigit})+
func matchHexinteger(l *lexer, i int) int { return matchRadixInteger(l.text, i, "xX", isHexDigit) }

// octinteger 0[oO](_?{octdigit})+
func matchOctinteger(l *lexer, i int) int { return matchRadixInteger(l.text, i, "oO", isOctDigit) }

// bininteger 0[bB](_?{bindigit})+
func matchBininteger(l *lexer, i int) int { return matchRadixInteger(l.text, i, "bB", isBinDigit) }

func matchRadixInteger(text string, i int, marks string, digit func(byte) bool) int {
	if text[i] != '0' || i+1 >= len(text) || strings.IndexByte(marks, text[i+1]) < 0 {
		return -1
	}
	j := i + 2
	for j < len(text) {
		if digit(text[j]) {
			j++
		} else if text[j] == '_' && j+1 < len(text) && digit(text[j+1]) {
			j += 2
		} else {
			break
		}
	}
	if j == i+2 {
		return -1
	}
	return j - i
}

// hexfail 0[xX]_?, octfail 0[oO]_?, binfail 0[bB]_?
func matchHexfail(l *lexer, i int) int { return matchRadixFail(l.text, i, "xX") }
func matchOctfail(l *lexer, i int) int { return matchRadixFail(l.text, i, "oO") }
func matchBinfail(l *lexer, i int) int { return matchRadixFail(l.text, i, "bB") }

func matchRadixFail(text string, i int, marks string) int {
	if text[i] != '0' || i+1 >= len(text) || strings.IndexByte(marks, text[i+1]) < 0 {
		return -1
	}
	if i+2 < len(text) && text[i+2] == '_' {
		return 3
	}
	return 2
}

// numeric (({decinteger}\.{decinteger}?)|(\.{decinteger}))
func matchNumeric(l *lexer, i int) int {
	if l.text[i] == '.' {
		if n := matchDecinteger(l, i+1); n > 0 {
			return 1 + n
		}
		return -1
	}
	whole := matchDecinteger(l, i)
	if whole < 0 || i+whole >= len(l.text) || l.text[i+whole] != '.' {
		return -1
	}
	n := whole + 1
	if part := matchDigits(l.text, i+n, isDigit); part > 0 {
		n += part
	}
	return n
}

// numericfail {decinteger}\.\.
func matchNumericfail(l *lexer, i int) int {
	whole := matchDecinteger(l, i)
	if whole < 0 || !strings.HasPrefix(l.text[i+whole:], "..") {
		return -1
	}
	return whole + 2
}

// real ({decinteger}|{numeric})[Ee][-+]?{decinteger}
func matchReal(l *lexer, i int) int {
	n := matchRealMantissa(l, i)
	if n < 0 {
		return -1
	}
	j := i + n
	if j >= len(l.text) || (l.text[j] != 'e' && l.text[j] != 'E') {
		return -1
	}
	j++
	if j < len(l.text) && (l.text[j] == '-' || l.text[j] == '+') {
		j++
	}
	exponent := matchDecinteger(l, j)
	if exponent < 0 {
		return -1
	}
	return j + exponent - i
}

// realfail ({decinteger}|{numeric})[Ee][-+]
func matchRealfail(l *lexer, i int) int {
	n := matchRealMantissa(l, i)
	if n < 0 {
		return -1
	}
	j := i + n
	if j >= len(l.text) || (l.text[j] != 'e' && l.text[j] != 'E') {
		return -1
	}
	j++
	if j >= len(l.text) || (l.text[j] != '-' && l.text[j] != '+') {
		return -1
	}
	return j + 1 - i
}

// matchRealMantissa is ({decinteger}|{numeric}), the longer of the two where both match.
func matchRealMantissa(l *lexer, i int) int {
	n := matchNumeric(l, i)
	if whole := matchDecinteger(l, i); whole > n {
		n = whole
	}
	return n
}

// integer_junk {decinteger}{identifier}
func matchIntegerJunk(l *lexer, i int) int { return matchJunk(l, i, matchDecinteger(l, i)) }

// numeric_junk {numeric}{identifier}
func matchNumericJunk(l *lexer, i int) int { return matchJunk(l, i, matchNumeric(l, i)) }

// real_junk {real}{identifier}
func matchRealJunk(l *lexer, i int) int { return matchJunk(l, i, matchReal(l, i)) }

// matchJunk is the longest match of a junk rule - {decinteger}{identifier},
// {numeric}{identifier}, {real}{identifier} - whose number matches n bytes at its longest. Flex
// matches a rule's whole pattern and takes the longest match of it, so the number's own longest
// match is not the only place the identifier may begin: an underscore is an {ident_start} as
// well as a {decinteger}'s separator, and every match of {decinteger} that stops before one is a
// match the identifier can follow. 1_0$$ is therefore not the number 1_0 and then $$, but
// {integer_junk} matching the number 1 and the identifier _0$$ - PostgreSQL 18.6 answers
// select 1_0$$ x $$ with "trailing junk after numeric literal" at the 1 and reads no
// dollar-quoted literal in it at all.
//
// Where such an underscore can be is one question for all three rules, and the answer is the
// number's trailing run of [0-9_]: the {decinteger} whose length can vary is the last one of the
// number, every byte before that run is a byte the number cannot end before ([.eE+-] and the
// digits ahead of them), and a {decinteger} neither begins nor ends on an underscore. So the
// identifier begins at the first underscore of that run, where the run holds one.
//
// The earliest such underscore gives the longest match of the whole rule, so there is nothing to
// search: every later end of the number is a byte of {ident_cont} inside the identifier that
// starts at that underscore, so the identifier starting there would end on the same byte.
func matchJunk(l *lexer, i, n int) int {
	if n < 0 {
		return -1
	}
	end := i + n
	run := end
	for run > i && (isDigit(l.text[run-1]) || l.text[run-1] == '_') {
		run--
	}
	if at := strings.IndexByte(l.text[run:end], '_'); at >= 0 {
		end = run + at
	}
	name := matchIdentifier(l, end)
	if name < 0 {
		return -1
	}
	return end + name - i
}

// param \${decdigit}+
func matchParam(l *lexer, i int) int {
	if l.text[i] != '$' {
		return -1
	}
	j := i + 1
	for j < len(l.text) && isDigit(l.text[j]) {
		j++
	}
	if j == i+1 {
		return -1
	}
	return j - i
}

// param_junk \${decdigit}+{identifier}. {decdigit}+ holds no underscore and a digit is no
// {ident_start}, so the identifier can only begin where the digits end and the longest match of
// the number is the only one to try.
func matchParamJunk(l *lexer, i int) int {
	n := matchParam(l, i)
	if n < 0 {
		return -1
	}
	name := matchIdentifier(l, i+n)
	if name < 0 {
		return -1
	}
	return n + name
}

// other . , which flex's dot matches every byte but a line feed with: flex's "." is [^\n], and
// scan.l's {newline} is [\n\r], so a carriage return is a byte "." takes. In INITIAL that costs
// nothing, since {whitespace} matches a carriage return and is the earlier rule; inside <xeu> it
// is the whole of the rule, because <xeu>. is what refuses a carriage return where the second
// half of a surrogate pair was wanted. PostgreSQL 18.6 answers select E'\ud83d<CR>\udc00' with
// "invalid Unicode surrogate pair at or near" that byte, and a reading that let <xeu>. refuse it
// matched no rule of that state at all and went on reading past a statement Postgres refused.
func matchOther(l *lexer, i int) int { return matchDot(l, i) }

func matchDot(l *lexer, i int) int {
	if l.text[i] == '\n' {
		return -1
	}
	return 1
}

func matchNewlineByte(l *lexer, i int) int {
	if l.text[i] == '\n' {
		return 1
	}
	return -1
}

// literal matches one fixed string, for the rules scan.l spells out: "::", "..", ":=", "=>",
// "<=", ">=", "<>", "!=", {xqdouble} and {xddouble}.
func literal(s string) func(*lexer, int) int {
	return func(l *lexer, i int) int {
		if strings.HasPrefix(l.text[i:], s) {
			return len(s)
		}
		return -1
	}
}

func runUntilByte(text string, i int, stop byte) int {
	j := i
	for j < len(text) && text[j] != stop {
		j++
	}
	if j == i {
		return -1
	}
	return j - i
}

// --- the actions, one per rule of scan.l's rules section ---

// {whitespace}: ignored by scan.l. A run of space is handed to the reader one byte at a time and
// a -- comment whole, which is the shape the three readers of scanSQL have always been given.
func actWhitespace(l *lexer, i, n int) {
	if l.text[i] == '-' {
		l.start = i
		l.ret(sqlComment, i+n)
		return
	}
	for j := i; j < i+n; j++ {
		l.emit(sqlToken{kind: sqlSpace, start: j, end: j + 1})
	}
	l.pos = i + n
}

// {xcstart}: SET_YYLLOC(); xcdepth = 0; BEGIN(xc); yyless(2).
func actXcstart(l *lexer, i, n int) {
	l.start = i
	l.depth = 0
	l.state = lexXC
	l.pos = i + 2
}

// <xc>{xcstart}: xcdepth++; yyless(2).
func actXcDeeper(l *lexer, i, n int) {
	l.depth++
	l.pos = i + 2
}

// <xc>{xcstop}: BEGIN(INITIAL) at depth 0, else xcdepth--.
func actXcstop(l *lexer, i, n int) {
	if l.depth <= 0 {
		l.ret(sqlComment, i+n)
		return
	}
	l.depth--
	l.pos = i + n
}

// {xbstart}: SET_YYLLOC(); BEGIN(xb).
func actXbstart(l *lexer, i, n int) { l.beginLiteral(i, n, lexXB, sqlString) }

// {xhstart}: SET_YYLLOC(); BEGIN(xh).
func actXhstart(l *lexer, i, n int) { l.beginLiteral(i, n, lexXH, sqlString) }

// {xnstart}: SET_YYLLOC(); yyless(1) - only the n is eaten, and NCHAR, or the identifier n where
// it is no keyword, is returned. The quote after it is read again from INITIAL, by {xqstart},
// which is why an N'…' literal takes standard_conforming_strings and its n is a word of its own.
func actXnstart(l *lexer, i, n int) {
	l.start = i
	l.ret(sqlWord, i+1)
}

// {xqstart}: warn_on_first_escape = true; saw_non_ascii = false; SET_YYLLOC();
// BEGIN(standard_conforming_strings ? xq : xe); startlit().
func actXqstart(l *lexer, i, n int) {
	l.sawNonASCII = false
	if l.standardConformingStrings {
		l.beginLiteral(i, n, lexXQ, sqlString)
	} else {
		l.beginLiteral(i, n, lexXE, sqlString)
	}
}

// {xestart}: warn_on_first_escape = false; saw_non_ascii = false; SET_YYLLOC(); BEGIN(xe);
// startlit().
func actXestart(l *lexer, i, n int) {
	l.sawNonASCII = false
	l.beginLiteral(i, n, lexXE, sqlString)
}

// {xusstart}: SET_YYLLOC(); ereport(ERROR, "unsafe use of string constant with Unicode escapes")
// when standard_conforming_strings is off; BEGIN(xus).
func actXusstart(l *lexer, i, n int) {
	l.beginLiteral(i, n, lexXUS, sqlUnicodeEscape)
	if !l.standardConformingStrings {
		l.fail()
	}
}

func (l *lexer) beginLiteral(i, n int, state lexState, kind sqlTokenKind) {
	l.start = i
	l.kind = kind
	l.state = state
	l.lit.Reset() // startlit()
	l.pos = i + n
}

// <xb,xh,xq,xe,xus>{quote}: state_before_str_stop = YYSTATE; BEGIN(xqs).
func actQuoteStop(l *lexer, i, n int) {
	l.before = l.state
	l.state = lexXQS
	l.pos = i + n
}

// <xqs>{quotecontinue}: BEGIN(state_before_str_stop) - the literal carries on, the whitespace,
// the newline and the comments inside the continuation belonging to its token.
func actQuotecontinue(l *lexer, i, n int) {
	l.state = l.before
	l.pos = i + n
}

// <xqs>{quotecontinuefail}, <xqs>{other}, <xqs><<EOF>>: yyless(0); BEGIN(INITIAL); return BCONST,
// XCONST, SCONST or USCONST for the state the literal was read in. The xq and xe arms check what
// the literal assembled before they return it - "Check that the data remains valid, if it might
// have been made invalid by unescaping any chars": "if (yyextra->saw_non_ascii)
// pg_verifymbstr(yyextra->literalbuf, yyextra->literallen, false)". That is where an escape
// which made a byte the server encoding cannot hold refuses the statement, and it refuses it at
// the literal's own start, the yylloc of this action being the one {xqstart} or {xestart} set:
// PostgreSQL 18.6 answers select e'\400' with "invalid byte sequence for encoding "UTF8": 0x00"
// and select e'\xff' with the same for 0xff.
func actQuoteStopFail(l *lexer, i, n int) {
	if (l.before == lexXQ || l.before == lexXE) && l.sawNonASCII && !verifymbstr(l.lit.String()) {
		l.fail()
	}
	l.ret(l.kind, i)
}

// verifymbstr is pg_verifymbstr: whether a literal's bytes are a string the server encoding can
// hold. The encoding is a third thing a query text does not carry, like the two settings the
// header names, and this reading takes UTF-8: the server_encoding of the PostgreSQL 18.6 this
// module is read against. A zero byte is refused whatever the encoding - pg_verify_mbstr_len
// stops on it before it asks the encoding anything - and a high byte is a string of its own
// under a single-byte encoding, so against a SQL_ASCII or LATIN1 server this reading refuses a
// literal that server would have read, and covers a tail Postgres went on past. That is the
// direction this report errs in everywhere else.
func verifymbstr(literal string) bool {
	return strings.IndexByte(literal, 0) < 0 && utf8.ValidString(literal)
}

// <xe>{xeunicode}: BEGIN(xeu) for the first half of a surrogate pair; yyerror("invalid Unicode
// surrogate pair") for a second half with no first; addunicode(c) for anything else.
func actXeunicode(l *lexer, i, n int) {
	c := hexValue(l.text[i+2 : i+n])
	switch {
	case c >= 0xD800 && c <= 0xDBFF:
		l.utf16First = c
		l.state = lexXEU
	case c >= 0xDC00 && c <= 0xDFFF:
		l.fail()
	default:
		l.addUnicode(c)
	}
	l.pos = i + n
}

// <xeu>{xeunicode}: yyerror("invalid Unicode surrogate pair") unless this is a second half;
// addunicode(surrogate_pair_to_codepoint(utf16_first_part, c)); BEGIN(xe).
func actXeuSecond(l *lexer, i, n int) {
	c := hexValue(l.text[i+2 : i+n])
	if c < 0xDC00 || c > 0xDFFF {
		l.fail()
	}
	l.addUnicode(0x10000 + (l.utf16First-0xD800)*0x400 + (c - 0xDC00))
	l.state = lexXE
	l.pos = i + n
}

// <xeu>. , <xeu>\n , <xeu><<EOF>>: yyerror("invalid Unicode surrogate pair").
func actXeuFail(l *lexer, i, n int) {
	l.fail()
	l.pos = i + n
}

// <xe,xeu>{xeunicodefail}: ereport(ERROR, "invalid Unicode escape").
func actXeunicodefail(l *lexer, i, n int) {
	l.fail()
	l.pos = i + n
}

func hexValue(s string) int {
	v := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isDigit(c):
			v = v*16 + int(c-'0')
		case 'a' <= c && c <= 'f':
			v = v*16 + int(c-'a') + 10
		default:
			v = v*16 + int(c-'A') + 10
		}
	}
	return v
}

// addunicode(c): "if (!is_valid_unicode_codepoint(c)) yyerror("invalid Unicode escape value")",
// which is 0 < c <= 0x10FFFF, and then the codepoint's bytes in the server encoding go into the
// literal - this reading's UTF-8, as verifymbstr reads it. "We expect that
// pg_unicode_to_server() will complain about any unconvertible code point, so we don't have to
// set saw_non_ascii": that complaint is the encoding's, and an encoding that holds every
// codepoint has none to make.
func (l *lexer) addUnicode(c int) {
	if c <= 0 || c > 0x10FFFF {
		l.fail()
		return
	}
	l.lit.WriteRune(rune(c))
}

// octValue reads {xeoctesc}'s digits as strtoul(yytext + 1, NULL, 8) reads them; the caller
// takes the result as the unsigned char addlitchar() is given, so \400 is a zero byte.
func octValue(s string) int {
	v := 0
	for i := range len(s) {
		v = v*8 + int(s[i]-'0')
	}
	return v
}

// <xq,xe,xus>{xqdouble}, whose action adds one quote byte to the literal:
//
//	addlitchar('\'');
//
// (in a block because gofmt reads two quotes in doc text as a closing typographic quote and
// rewrites them, which is how this line came to carry a U+201D). Only <xe> is wired to it here,
// because the <xqs> action reads what an <xe> literal assembled and no reader of this file asks
// for what the other two assemble; their {xqdouble} is actSkip, which reads the same two bytes
// and keeps nothing.
func actXqdouble(l *lexer, i, n int) {
	l.lit.WriteByte('\'')
	l.pos = i + n
}

// actAddLit is addlit(yytext, yyleng): <xe>{xeinside}, and <xe>. , which scan.l keeps "only for
// \ just before EOF".
func actAddLit(l *lexer, i, n int) {
	l.lit.WriteString(l.text[i : i+n])
	l.pos = i + n
}

// <xe>{xeescape}: addlitchar(unescape_single_char(yytext[1])), which is \b \f \n \r \t \v for
// those six letters and the byte itself for anything else, with "if (c == '\0' ||
// IS_HIGHBIT_SET(c)) saw_non_ascii = true" for the byte it added. backslash_quote, which refuses
// \' here when it is off, is not read: the header says why.
func actXeescape(l *lexer, i, n int) {
	c := l.text[i+1]
	switch c {
	case 'b':
		c = '\b'
	case 'f':
		c = '\f'
	case 'n':
		c = '\n'
	case 'r':
		c = '\r'
	case 't':
		c = '\t'
	case 'v':
		c = '\v'
	}
	l.addEscaped(i, n, c)
}

// <xe>{xeoctesc}: addlitchar(strtoul(yytext + 1, NULL, 8)).
func actXeoctesc(l *lexer, i, n int) { l.addEscaped(i, n, byte(octValue(l.text[i+1:i+n]))) }

// <xe>{xehexesc}: addlitchar(strtoul(yytext + 2, NULL, 16)).
func actXehexesc(l *lexer, i, n int) { l.addEscaped(i, n, byte(hexValue(l.text[i+2:i+n]))) }

// addEscaped adds the byte an escape made to the literal and, where that byte is not 7-bit
// ASCII, records what the three escape rules record: "if (c == '\0' || IS_HIGHBIT_SET(c))
// yyextra->saw_non_ascii = true", which is what makes the <xqs> action verify the literal.
func (l *lexer) addEscaped(i, n int, c byte) {
	l.lit.WriteByte(c)
	if c == 0 || c >= 0x80 {
		l.sawNonASCII = true
	}
	l.pos = i + n
}

// {dolqdelim}: SET_YYLLOC(); dolqstart = yytext; BEGIN(xdolq).
func actDolqdelim(l *lexer, i, n int) {
	l.start = i
	l.kind = sqlDollarString
	l.dolq = l.text[i : i+n]
	l.state = lexXDolq
	l.pos = i + n
}

// {dolqfailed}: SET_YYLLOC(); yyless(1); return yytext[0] - the $ alone, as {other} would.
func actDolqfailed(l *lexer, i, n int) {
	l.start = i
	l.ret(sqlOther, i+1)
}

// <xdolq>{dolqdelim}: the literal ends where its own delimiter repeats; another delimiter has its
// last $ thrown back with yyless(yyleng - 1) in case it opens the closing one.
func actXdolqDelim(l *lexer, i, n int) {
	if l.text[i:i+n] == l.dolq {
		l.ret(sqlDollarString, i+n)
		return
	}
	l.pos = i + n - 1
}

// {xdstart}: SET_YYLLOC(); BEGIN(xd).
func actXdstart(l *lexer, i, n int) {
	l.start = i
	l.kind = sqlQuotedIdentifier
	l.lit.Reset()
	l.state = lexXD
	l.pos = i + n
}

// {xuistart}: SET_YYLLOC(); BEGIN(xui). U&"…" is a name, not a value, but its escapes can spell
// any name, so the readers are given it as its own kind rather than as a quoted identifier.
func actXuistart(l *lexer, i, n int) {
	l.start = i
	l.kind = sqlUnicodeEscape
	l.lit.Reset()
	l.state = lexXUI
	l.pos = i + n
}

// {xufailed}: SET_YYLLOC(); yyless(1); the u is returned as {identifier} would return it.
func actXufailed(l *lexer, i, n int) {
	l.start = i
	l.ret(sqlWord, i+1)
}

// <xd>{xdstop}, <xui>{dquote}: BEGIN(INITIAL); return IDENT or UIDENT. A zero-length name is
// yyerror("zero-length delimited identifier").
func actXdstop(l *lexer, i, n int) {
	if l.lit.Len() == 0 {
		l.fail()
	}
	l.ret(l.kind, i+n)
}

// <xd,xui>{xddouble}: addlitchar('"').
func actXddouble(l *lexer, i, n int) {
	l.lit.WriteByte('"')
	l.pos = i + n
}

// <xd,xui>{xdinside}: addlit(yytext).
func actXdinside(l *lexer, i, n int) {
	l.lit.WriteString(l.text[i : i+n])
	l.pos = i + n
}

// actSkip is the action of every rule that only adds to the literal it is inside or ignores what
// it read: <xc>{xcinside}, <xc>{op_chars}, <xc>\*+, <xb>{xbinside}, <xh>{xhinside},
// <xq,xus>{xqdouble}, <xq,xus>{xqinside}, <xdolq>{dolqinside}, <xdolq>{dolqfailed} and <xdolq>.
// The addlit of a rule of <xe> is kept rather than skipped (actAddLit and its kin), because the
// <xqs> action reads what an <xe> literal assembled; what the other states assemble no reader of
// this file asks for, <xq>'s literal included, saw_non_ascii being false for every literal <xq>
// reads.
func actSkip(l *lexer, i, n int) { l.pos = i + n }

// actOneToken returns the action of a rule that returns one token of the text it matched.
func actOneToken(kind sqlTokenKind) func(*lexer, int, int) {
	return func(l *lexer, i, n int) {
		l.start = i
		l.ret(kind, i+n)
	}
}

// {param}: "val = pg_strtoint32_safe(yytext + 1, (Node *) &escontext); if
// (escontext.error_occurred) yyerror("parameter number too large")" - the digits after the $ are
// read as an int32, and a number no int32 holds is refused where it stands, the parameter's own
// first byte. PostgreSQL 18.6 answers select $999999999999 with "parameter number too large" at
// the $.
func actParam(l *lexer, i, n int) {
	l.start = i
	if _, err := strconv.ParseInt(l.text[i+1:i+n], 10, 32); err != nil {
		l.fail()
	}
	l.ret(sqlParam, i+n)
}

// actJunk returns the action of a rule scan.l raises an error from and reads nothing further
// after: {param_junk}, {hexfail}, {octfail}, {binfail}, {realfail}, {integer_junk},
// {numeric_junk} and {real_junk}. Postgres stops here; lexSQL records where and reads on, so that
// a reader which only wants tokens is not cut short by a text Postgres would have refused.
func actJunk(kind sqlTokenKind) func(*lexer, int, int) {
	return func(l *lexer, i, n int) {
		l.start = i
		l.fail()
		l.ret(kind, i+n)
	}
}

// {numericfail}: yyless(yyleng - 2) throws the .. back and the digits are returned as an integer,
// so that 1..10 lexes as 1, dot_dot, 10.
func actNumericfail(l *lexer, i, n int) {
	l.start = i
	l.ret(sqlNumber, i+n-2)
}

// namedatalen is NAMEDATALEN, "#define NAMEDATALEN 64" in src/include/pg_config_manual.h.
const namedatalen = 64

// {operator}: the Op rule, which stops the token at a slash-star or a dash-dash inside it and
// throws back a trailing + or - unless the operator holds one of ~!@#^&|`?% before it, and then
// refuses what is left where that is NAMEDATALEN bytes or more: "Complain if operator is too
// long. Unlike the case for identifiers, we make this an error not a notice-and-truncate,
// because the odds are we are looking at a syntactic mistake anyway. if (nchars >= NAMEDATALEN)
// yyerror("operator too long")". Where the token stops, whether a qualifying character comes
// before its last byte and where its trailing run of [+-] begins are answers about the run of
// {op_chars} rather than about this token, and opRun holds them.
func actOperator(l *lexer, i, n int) {
	run := l.opRunAt(i)
	nchars := run.stop - i // "if (nchars < yyleng) yyless(nchars)", at the first "/*" or "--"
	if nchars > 1 && (l.text[i+nchars-1] == '+' || l.text[i+nchars-1] == '-') && run.qualify < i {
		// "didn't find a qualifying character, so remove all trailing [+-]": the token ends
		// where that run of [+-] begins, or is one byte long where it begins at or before the
		// token's own first byte.
		nchars = max(1, run.plus-i)
	}
	l.start = i
	if nchars >= namedatalen {
		l.fail()
	}
	l.ret(sqlOther, i+nchars)
}
