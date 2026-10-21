package pmdoc

import (
	"bytes"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// linkify links bare URLs and email addresses (linkifyGuard), at the priority goldmark's
// extension.Linkify gives its parser.
type linkify struct{}

func (linkify) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(util.Prioritized(newLinkifyGuard(), 999)))
}

// linkifyGuard reads a www or http(s) URL where the browser editor's parser reads one, and runs
// goldmark's linkify parser for an email address only where one can match.
//
// A URL is read as micromark-extension-gfm-autolink-literal's tokenizer reads it (literalURL),
// not by goldmark's pattern, which ends a URL elsewhere: it keeps a `"`, `'`, `;`, `]` or `_`
// ending the URL and a `)` after an earlier trimmed character inside it, and stops at a backslash,
// an asterisk or a character outside ASCII, where GFM's extended autolink ends a URL only at
// whitespace or `<` and leaves out the trailing punctuation before it. It begins where that
// tokenizer begins one (literalURLFollows) and goldmark asks an inline parser: after whitespace or
// ASCII punctuation, and at the head of a line or of the text after another inline node. goldmark
// asks after no letter, digit or character outside ASCII, so a URL right after a digit or a
// curly quote, which the editor links, stays text here.
//
// goldmark's parser, triggered at every space, `*`, `_`, `~` and `(`, scans forward over email
// local-part characters to the next `@` before it knows there is no email there: on a line of
// `a_a_a_…` every `_` scans to the line's end, and the line costs time quadratic in its length
// (LEGION-465). The guard reads each line once, recording for each `@` where its run of local-part
// characters begins, and answers a trigger with no `@` reachable through local-part characters, or
// one whose `@` the parser already refused, without calling it. What the parser links is
// unchanged: it is called exactly where it could link, and whether it links an email ending at an
// `@` does not depend on which trigger before that `@` asked. A line whose segment carries padding
// (the columns of a tab its container took part of) goes to the parser as it is, since its bytes
// are not the source's from the segment's start; that is one trigger, at the line's head.
type linkifyGuard struct {
	inner parser.InlineParser
}

func newLinkifyGuard() *linkifyGuard {
	return &linkifyGuard{inner: extension.NewLinkifyParser()}
}

var (
	linkifyLineKey   = parser.NewContextKey()
	literalDomainKey = parser.NewContextKey()
)

// linkifyLine is what the guard knows about the line ending at stop in the source, from the first
// offset a trigger asked about (from) to its end.
type linkifyLine struct {
	stop   int
	from   int
	ats    []int  // offsets of each `@`, ascending
	starts []int  // for each `@`, the lowest offset ≥ from from which only local-part characters lead to it
	failed []bool // for each `@`, whether the parser refused an email ending at it
}

var (
	// emailLocal marks the bytes goldmark's util.FindEmailIndex reads as an email's local part
	// (its emailTable); linkify_test.go holds the two to the same set. A line opening with `ftp:`
	// reaches no `@` through them, so goldmark's parser never reads its URL.
	emailLocal = func() (table [256]bool) {
		for _, c := range []byte("!#$%&'*+-./0123456789=?ABCDEFGHIJKLMNOPQRSTUVWXYZ^_`abcdefghijklmnopqrstuvwxyz{|}~") {
			table[c] = true
		}
		return table
	}()
)

// linkifyTriggers is where the guard is asked: at whitespace and a line's head (` `), as goldmark's
// parser is, and at every ASCII punctuation character, which a URL may follow, but `&`, `#`, `;` and
// `\`. goldmark ends the text before each character it asks a parser about, and the reader decodes
// character references one text at a time, so a reference split there would not be decoded.
var linkifyTriggers = func() []byte {
	triggers := []byte{' '}
	for c := byte('!'); c <= '~'; c++ {
		if isASCIIPunctuation(c) && !strings.ContainsRune(`&#;\`, rune(c)) {
			triggers = append(triggers, c)
		}
	}
	return triggers
}()

func (g *linkifyGuard) Trigger() []byte { return linkifyTriggers }

func (g *linkifyGuard) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	if pc.IsInLinkLabel() {
		return nil
	}
	line, segment := block.PeekLine()
	c := line[0]
	goldmarks := !isASCIIPunctuation(c) || c == '*' || c == '_' || c == '~' || c == '('
	if segment.Padding != 0 {
		if !goldmarks {
			return nil
		}
		return g.inner.Parse(parent, block, pc)
	}
	if url, ok := g.literalURLNode(parent, block, pc, line, segment); ok {
		return url
	}
	if !goldmarks {
		// goldmark's parser is not asked here.
		return nil
	}
	start := segment.Start
	rest := line
	// The parser skips the trigger where it is not a line head, as its Parse does.
	if c == ' ' || c == '*' || c == '_' || c == '~' || c == '(' {
		start++
		rest = rest[1:]
	}
	if len(rest) == 0 || util.IsPunct(rest[0]) {
		return nil
	}
	state := g.line(block.Source(), start, segment.Stop, pc)
	index := sort.SearchInts(state.ats, start)
	if index == len(state.ats) || state.starts[index] > start || state.failed[index] {
		return nil
	}
	node := g.inner.Parse(parent, block, pc)
	if node == nil {
		state.failed[index] = true
	}
	return node
}

// line is the guard's record of the line ending at stop, read from `from` on first sight. Triggers
// on one line ask in offset order, so a record read from an earlier offset answers every later one.
func (g *linkifyGuard) line(source []byte, from, stop int, pc parser.Context) *linkifyLine {
	if state, ok := pc.Get(linkifyLineKey).(*linkifyLine); ok && state.stop == stop && state.from <= from {
		return state
	}
	state := &linkifyLine{stop: stop, from: from}
	runStart := from
	for offset := from; offset < stop && offset < len(source); offset++ {
		c := source[offset]
		if c == '@' {
			state.ats = append(state.ats, offset)
			state.starts = append(state.starts, runStart)
			state.failed = append(state.failed, false)
			runStart = offset + 1
			continue
		}
		if !emailLocal[c] {
			runStart = offset + 1
		}
	}
	pc.Set(linkifyLineKey, state)
	return state
}

func (g *linkifyGuard) CloseBlock(parent ast.Node, pc parser.Context) {}

// literalURLNode reads the URL that begins after the trigger opening line, or at line's start where
// that is no trigger, as an autolink, appending the trigger before it as text. It reports false
// where no URL begins there; nil where the browser editor's parser reads none from that beginning.
// That parser reads a URL its tokenizer refuses there again from the text it leaves (textURL);
// where the markdown cannot say what that text is, the URL is read as goldmark's parser reads it.
func (g *linkifyGuard) literalURLNode(parent ast.Node, block text.Reader, pc parser.Context, line []byte, segment text.Segment) (ast.Node, bool) {
	// The guard sends a line whose segment carries padding to goldmark's parser, so line is the
	// source's bytes from segment.Start.
	start, rest, previous, trigger := segment.Start, line, block.PrecendingCharacter(), 0
	if c := line[0]; util.IsSpace(c) || isASCIIPunctuation(c) {
		start, rest, previous, trigger = start+1, line[1:], rune(c), 1
	}
	if !literalURLPrefix(rest) {
		return nil, false
	}
	length, www := 0, false
	if literalURLFollows(previous, rest[3] == '.') {
		length, www = literalURL(rest, literalDomainsOf(pc, start, segment.Stop))
	}
	if length == 0 {
		var decided bool
		if length, www, decided = textURL(rest); !decided {
			return g.inner.Parse(parent, block, pc), true
		}
	}
	if length == 0 {
		return nil, true
	}
	if trigger != 0 {
		ast.MergeOrAppendTextSegment(parent, segment.WithStop(start))
	}
	block.Advance(trigger + length)
	link := ast.NewAutoLink(ast.AutoLinkURL, ast.NewTextSegment(text.NewSegment(start, start+length)))
	if www {
		link.Protocol = []byte("http")
	}
	return link, true
}

// readsURLAt reports whether linkifyGuard reads a URL from value[start:], text written outside a
// link, read to its line's end: at the text's start, whose character before the writer does not
// judge, or after a character the guard is asked at (linkifyTriggers) that is whitespace or ASCII
// punctuation, but `[`, which opens a link label, where the guard reads nothing. Where textURL
// cannot decide, goldmark's parser reads no URL with a scheme in another case than lowercase, and
// a www URL there is left as main wrote it.
func readsURLAt(value string, start int) bool {
	var previous rune = '\n'
	if start > 0 {
		previous = rune(value[start-1])
		if c := value[start-1]; !(util.IsSpace(c) || isASCIIPunctuation(c)) || strings.IndexByte(`&#;\[`, c) >= 0 {
			return false
		}
	}
	rest := []byte(value[start:])
	if end := bytes.IndexByte(rest, '\n'); end >= 0 {
		rest = rest[:end]
	}
	if !literalURLPrefix(rest) {
		return false
	}
	if literalURLFollows(previous, rest[3] == '.') {
		if length, _ := literalURL(rest, &literalDomains{from: -1, end: -1}); length > 0 {
			return true
		}
	}
	length, _, _ := textURL(rest)
	return length > 0
}

// literalURLFollows reports whether the tokenizer begins a URL after previous, the character before
// it or a line feed at a line's start (previousProtocol, previousWww): a protocol after anything but
// an ASCII letter, and a www literal after a space, a tab, a line ending, `(`, `*`, `_`, `[`, `]` or
// `~`.
func literalURLFollows(previous rune, www bool) bool {
	if !www {
		return previous >= utf8.RuneSelf || !isASCIILetter(byte(previous))
	}
	return strings.ContainsRune(" \t\n\r(*_[]~", previous)
}

// textURL is the length of the URL mdast-util-gfm-autolink-literal's transform reads from the text
// opening with rest, and whether it is a www URL; or 0 where it reads none. The browser editor's
// parser runs that transform over the text its tokenizer left, so it links a URL the tokenizer
// refused: a host opening with `-` or `.`, a www URL after a character other than whitespace, `(`,
// `*`, `_`, `[`, `]` or `~`, and `www.` ending a paragraph. The transform reads a URL after
// whitespace, punctuation or the start of a text, which every place the guard is asked satisfies.
//
// It reads decoded text, where escapes and character references are their characters and any
// other markup has ended the text, and the guard reads markdown: where the URL's markdown holds a
// backslash, `&`, `*`, `~`, a backtick, `<`, `[`, `]`, `!` or an underscore beside punctuation or
// whitespace, which may be markup, it reports that it cannot decide (decided false).
func textURL(rest []byte) (length int, www, decided bool) {
	// (https?:\/\/|www(?=\.))([-.\w]+)([^ \t\r\n]*), in any case.
	var protocol int
	switch {
	case len(rest) >= 8 && bytes.EqualFold(rest[:8], []byte("https://")):
		protocol = 8
	case len(rest) >= 7 && bytes.EqualFold(rest[:7], []byte("http://")):
		protocol = 7
	case len(rest) >= 4 && bytes.EqualFold(rest[:4], []byte("www.")):
	default:
		return 0, false, true
	}
	www = protocol == 0
	domainEnd := protocol
	for domainEnd < len(rest) && (isASCIIAlphaNumeric(rest[domainEnd]) || strings.IndexByte("-._", rest[domainEnd]) >= 0) {
		domainEnd++
	}
	if domainEnd == protocol || www && domainEnd == len("www") || !textURLDomain(string(rest[protocol:domainEnd])) {
		return 0, false, true
	}
	end := domainEnd
	for end < len(rest) && strings.IndexByte(" \t\r\n", rest[end]) < 0 {
		end++
	}
	for index := protocol; index < end; index++ {
		c := rest[index]
		if strings.IndexByte("\\&*~`<[]!", c) >= 0 ||
			c == '_' && (index == 0 || index+1 == end || !isASCIIAlphaNumeric(rest[index-1]) || !isASCIIAlphaNumeric(rest[index+1])) {
			return 0, false, false
		}
	}
	// splitUrl: trailing `!"&'),.:;<>?]}` leave the URL, but for each `)` closing a `(` before it.
	url := end
	for url > protocol && strings.IndexByte(`!"&'),.:;<>?]}`, rest[url-1]) >= 0 {
		url--
	}
	opened, closed := bytes.Count(rest[:url], []byte("(")), bytes.Count(rest[:url], []byte(")"))
	for trail := url; opened > closed; closed++ {
		next := bytes.IndexByte(rest[trail:end], ')')
		if next < 0 {
			break
		}
		url, trail = trail+next+1, trail+next+1
	}
	if url == protocol {
		return 0, false, true
	}
	return url, www, true
}

// textURLDomain is the transform's isCorrectDomain: at least two dot-separated parts, and neither
// of the last two, where not empty, holding an underscore or no letter or digit.
func textURLDomain(domain string) bool {
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts[len(parts)-2:] {
		if part != "" && (strings.Contains(part, "_") || strings.IndexFunc(part, func(char rune) bool {
			return char < utf8.RuneSelf && isASCIIAlphaNumeric(byte(char))
		}) < 0) {
			return false
		}
	}
	return true
}

// literalURLPrefix reports whether text opens as a www or http(s) autolink literal does, in any
// case: `www.`, or a protocol of letters ending in a colon.
func literalURLPrefix(text []byte) bool {
	return len(text) >= 4 && bytes.EqualFold(text[:4], []byte("www.")) ||
		len(text) >= 5 && bytes.EqualFold(text[:5], []byte("http:")) ||
		len(text) >= 6 && bytes.EqualFold(text[:6], []byte("https:"))
}

// literalURL is the length in bytes of the www or http(s) autolink literal at the start of text, as
// micromark-extension-gfm-autolink-literal's tokenizer reads one, and whether it is a www literal,
// whose href takes an `http://` before it; or 0 where that tokenizer reads none. text ends where its
// line does. A protocol is `http` or `https` in any case followed by `://` and a character neither
// whitespace, a control character nor punctuation; then come the domain and the path.
func literalURL(text []byte, domains *literalDomains) (int, bool) {
	if !literalURLPrefix(text) {
		return 0, false
	}
	www := text[3] == '.'
	at := 0
	if !www {
		at = bytes.IndexByte(text, ':') + 1
		if !bytes.HasPrefix(text[at:], []byte("//")) {
			return 0, false
		}
		at += 2
		if at == len(text) {
			return 0, false
		}
		if char, _ := utf8.DecodeRune(text[at:]); char < ' ' || char == 0x7f || literalEnd(char) || literalPunctuation(char) {
			return 0, false
		}
	} else if len(text) == len("www.") {
		// The paragraph ends after the `www.`; a line ending would be something after it.
		return 0, false
	}
	trail := literalTrail{text: text}
	end, ok := domains.read(text, at, &trail)
	if !ok {
		return 0, false
	}
	return literalPath(text, end, &trail), www
}

// literalDomains is the record of the last domain read on a line (tokenizeDomain), for every later
// www literal starting inside it: a domain ends at the first character that ends one, wherever it
// started, so only which underscores and dots count moves with its start. Without it a line of
// `_www.a_www.a…` would read its one long domain again from each trigger.
type literalDomains struct {
	stop      int // the source offset the line ends at
	base      int // the source offset text[0] stands at for the current read
	from, end int // source offsets: where the recorded domain read began and ended
	lastDot   int // the last dot inside it, or -1
	dotBefore int // the dot before that, or -1
	// underscore holds the last underscore after lastDot and the last between dotBefore and
	// lastDot, each -1 where there is none.
	underscore [2]int
	plain      int // the last character neither a dot nor an underscore, or -1
}

// literalDomainsOf is the line's domain record, kept across the triggers of one line, with the
// text a trigger reads starting at source offset start.
func literalDomainsOf(pc parser.Context, start, stop int) *literalDomains {
	domains, ok := pc.Get(literalDomainKey).(*literalDomains)
	if !ok || domains.stop != stop {
		domains = &literalDomains{stop: stop, from: -1, end: -1}
		pc.Set(literalDomainKey, domains)
	}
	domains.base = start
	return domains
}

// read reads the domain from text[at] (tokenizeDomain), reporting where it ends and whether it is
// one: something was read, and neither of its last two dot-separated segments holds an underscore.
// A dot or an underscore followed only by trailing punctuation up to an end ends it (literalTrail),
// as does whitespace or punctuation other than `-`.
func (d *literalDomains) read(text []byte, at int, trail *literalTrail) (int, bool) {
	from := d.base + at
	if d.from < 0 || from < d.from || from >= d.end {
		d.from, d.lastDot, d.dotBefore, d.plain = from, -1, -1, -1
		d.underscore = [2]int{-1, -1}
		i := at
		for i < len(text) {
			char, width := utf8.DecodeRune(text[i:])
			if char == '.' || char == '_' {
				if trail.ends(i) {
					break
				}
				if char == '_' {
					d.underscore[0] = d.base + i
				} else {
					d.dotBefore, d.lastDot = d.lastDot, d.base+i
					d.underscore[1], d.underscore[0] = d.underscore[0], -1
				}
				i++
				continue
			}
			if literalEnd(char) || char != '-' && literalPunctuation(char) {
				break
			}
			d.plain = d.base + i
			i += width
		}
		d.end = d.base + i
	}
	underscoreLast := d.underscore[0] >= max(from, d.lastDot+1)
	underscoreBefore := d.lastDot >= from && d.underscore[1] >= max(from, d.dotBefore+1)
	if underscoreLast || underscoreBefore || d.plain < from {
		return 0, false
	}
	return d.end - d.base, true
}

// literalPath is where the path from text[at] ends (tokenizePath): at whitespace, at `<`, or at a
// run of trailing punctuation followed by an end (literalTrail). A `)` with fewer `)` than `(`
// before it in the path is part of it whatever follows.
func literalPath(text []byte, at int, trail *literalTrail) int {
	opened, closed := 0, 0
	i := at
	for i < len(text) {
		char, width := utf8.DecodeRune(text[i:])
		switch {
		case char == '(':
			opened++
		case char == ')' && closed < opened:
			closed++
		case literalPathPunctuation(char):
			if trail.ends(i) {
				return i
			}
			if char == ')' {
				closed++
			}
		case literalEnd(char):
			return i
		}
		i += width
	}
	return i
}

// literalTrail answers whether the trailing punctuation from an offset of text is followed by an
// end, so that none of it is the URL's (tokenizeTrail): a run of `!"')*,.:;?_~`, a character
// reference's spelling (`&`, letters, `;`) and `]`, ended by whitespace, `<` or the line's end, or
// by `(` or `[` right after a `]`. It records the run it last found followed by something else:
// every offset the URL's reader asks about inside that run continues it the same way, so a long run
// is read once.
type literalTrail struct {
	text       []byte
	runStart   int
	runDecided int // the run's offsets from runStart up to here are not a trail
}

func (t *literalTrail) ends(at int) bool {
	if at >= t.runStart && at < t.runDecided {
		return false
	}
	text := t.text
	i := at
	for i < len(text) {
		switch char, _ := utf8.DecodeRune(text[i:]); {
		case literalTrailPunctuation(char):
			i++
		case char == '&':
			j := i + 1
			for j < len(text) && isASCIILetter(text[j]) {
				j++
			}
			if j == i+1 || j == len(text) || text[j] != ';' {
				t.runStart, t.runDecided = at, i+1
				return false
			}
			i = j + 1
		case char == ']':
			i++
			if i == len(text) || text[i] == '(' || text[i] == '[' {
				return true
			}
			if next, _ := utf8.DecodeRune(text[i:]); literalEnd(next) {
				return true
			}
		case char == '<' || literalEnd(char):
			return true
		default:
			t.runStart, t.runDecided = at, i
			return false
		}
	}
	return true
}

// literalTrailPunctuation is the punctuation a trail runs over.
func literalTrailPunctuation(char rune) bool {
	return char < utf8.RuneSelf && bytes.IndexByte([]byte(`!"')*,.:;?_~`), byte(char)) >= 0
}

// literalPathPunctuation is the punctuation at which the path asks whether a trail begins.
func literalPathPunctuation(char rune) bool {
	return char < utf8.RuneSelf && bytes.IndexByte([]byte(`!"&')*,.:;<?]_~`), byte(char)) >= 0
}

// literalEnd reports whether char ends a URL as whitespace: a line ending, a space or a tab, or
// what JavaScript's `\s` matches, which micromark reads as Unicode whitespace.
func literalEnd(char rune) bool {
	switch char {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return 0x2000 <= char && char <= 0x200a
}

// literalPunctuation reports whether char is punctuation or a symbol (`\p{P}` or `\p{S}`), as
// micromark classifies one UTF-16 code unit: a character outside the Basic Multilingual Plane is
// two surrogates to it, neither of which is punctuation.
func literalPunctuation(char rune) bool {
	return char <= 0xffff && (unicode.IsPunct(char) || unicode.IsSymbol(char))
}

func isASCIILetter(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}
