package pmdoc

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// linkify reads bare web addresses and email addresses as the browser editor's engine reads them
// (linkifyGuard).
type linkify struct{}

func (linkify) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(linkifyParsers()...))
}

// linkifyParsers is the readers' parsers of bare addresses: the guard, asked after every other
// inline parser, and the email reader at `_` (underscoreEmail), asked before emphasis.
func linkifyParsers() []util.PrioritizedValue {
	return []util.PrioritizedValue{util.Prioritized(linkifyGuard{}, 999), util.Prioritized(underscoreEmail{}, 450)}
}

// linkifyGuard reads a bare web address or email address wherever the browser editor's engine,
// remark-parse with remark-gfm, reads one, and as it reads it. The engine has two readers of them.
//
// micromark-extension-gfm-autolink-literal's tokenizer reads the markdown as it is written, before
// emphasis, code or links form, so an address it reads claims its bytes, a `*` or `_` among them.
// At each character it tries an email first, at a character of `[A-Za-z0-9+\-._]` whose previous
// character is neither one of them nor `/` (literalEmail); then, at an `h` after anything but an
// ASCII letter, a protocol URL, and at a `w` after a line's start, whitespace, `(`, `*`, `_`, `[`,
// `]` or `~`, a www URL (literalURL). The previous character is the one written before, the escaped
// one after a backslash escape and the `;` after a character reference; a line's or a table
// cell's first character has none before it. Inside a link label not yet closed it reads nothing.
// The guard reads the same way inside goldmark's inline parse: it is asked at every unescaped ASCII
// punctuation character and at whitespace and each head, after every other parser declines, and
// from there reads up to the next character goldmark asks at, where the tokenizer could start an
// address only after what that stretch holds. An address it starts there is an autolink, the text
// before it text. At `_`, where the tokenizer tries an email before emphasis, underscoreEmail reads
// it before goldmark's emphasis parser.
//
// mdast-util-gfm-autolink-literal's transform then searches each text node the tokenizer left,
// outside links, decoded: its URL pattern over every one first, then its email pattern over the
// text left, each refusal searched again one character later (mdast-util-find-and-replace). That
// is each block's CloseBlock's (linkTextAddresses), over each run of its text nodes between other
// nodes; autolink_transform.go holds it, and this file the tokenizer.
//
// Asking the guard at a character no other parser is asked at ends the text goldmark is building
// there: a line asked at a `.` and nowhere else becomes two texts. The guard records the last such
// cut with where goldmark would have begun the line's last text otherwise, and gives the split text
// back (linkifyBlock.settle) at the next character goldmark asks at, where the elements counter or
// the guard is asked first, and at CloseBlock: a markdown holding no address makes goldmark's tree
// without the guard, and is counted and weighed as it.
//
// Each character is read once: the guard's stretch ends at the next character goldmark asks at,
// an email starts only at the first of a run of its characters, a refused URL's domain and trail
// are recorded for the next read starting inside them (literalDomains, literalTrail), and the
// transform keeps the run a refused match read (urlDomain, emailRun).
type linkifyGuard struct{}

// linkifyTriggers is every ASCII punctuation character and a space, which stands for whitespace
// and a line's head.
var linkifyTriggers = func() []byte {
	triggers := []byte{' '}
	for c := byte('!'); c <= '~'; c++ {
		if util.IsPunct(c) {
			triggers = append(triggers, c)
		}
	}
	return triggers
}()

func (linkifyGuard) Trigger() []byte { return linkifyTriggers }

var (
	linkifyBlockKey  = parser.NewContextKey()
	literalDomainKey = parser.NewContextKey()
)

// linkifyBlock is what the guard knows of the textblock goldmark is reading.
type linkifyBlock struct {
	parent ast.Node
	// asked is the offset the guard was last asked at, -1 before it is first asked.
	asked int
	// head is where goldmark began the text it is building, as it would were the guard asked only
	// where another parser is: at a head or a character another parser reads.
	head int
	// cut is where goldmark last ended a text only because the guard was asked there, until it is
	// settled, or -1; cutHead is head then.
	cut, cutHead int
}

// linkifyBlockOf is the guard's record of parent, the textblock goldmark is reading.
func linkifyBlockOf(pc parser.Context, parent ast.Node) *linkifyBlock {
	state, _ := pc.Get(linkifyBlockKey).(*linkifyBlock)
	if state == nil || state.parent != parent {
		state = &linkifyBlock{parent: parent, asked: -1, cut: -1}
		pc.Set(linkifyBlockKey, state)
	}
	return state
}

func (linkifyGuard) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	_, segment := block.PeekLine()
	state := linkifyBlockOf(pc, parent)
	state.settle()
	at := segment.Start
	// goldmark has just ended a text here, rather than reached a head: its last text ends here and
	// already held where the guard was last asked. A text another parser made ending here began
	// after that.
	last, isText := parent.LastChild().(*ast.Text)
	ended := isText && last.Segment.Stop == at && last.Segment.Start <= state.asked
	state.asked, state.cut = at, -1
	if !pc.IsInLinkLabel() {
		if link := readLiteralAddress(parent, block, pc, segment); link != nil {
			return link
		}
	}
	asked := byte(' ')
	if segment.Padding == 0 {
		asked = block.Source()[at]
	}
	if !ended || util.IsSpace(asked) || bytes.IndexByte(inlineTriggers, asked) >= 0 {
		state.head = at
		return nil
	}
	state.cut, state.cutHead = at, state.head
	return nil
}

// settle gives the text goldmark split off at the guard's last cut, once it has ended the line
// there, back to the text before it, or, where goldmark would have begun the line's last text at a
// later head, moves the split there; a question later on the line takes the text past the cut, so
// goldmark splits nothing there. A split text is the textblock's last child until the next
// character goldmark asks at, where the elements counter or the guard is asked first, so each split
// is settled before anything counts or reads it - but for the empty texts a line holding nothing
// but a hard break's backslash ends with, which goldmark asks nothing on.
func (state *linkifyBlock) settle() {
	if state.cut < 0 {
		return
	}
	after, ok := state.parent.LastChild().(*ast.Text)
	for ok && after.Segment.IsEmpty() {
		after, ok = after.PreviousSibling().(*ast.Text)
	}
	if !ok || after.Segment.Start != state.cut {
		return
	}
	before, ok := after.PreviousSibling().(*ast.Text)
	if !ok || before.Segment.Stop != state.cut || before.SoftLineBreak() || before.HardLineBreak() {
		return
	}
	state.cut = -1
	if head := state.cutHead; head > before.Segment.Start {
		before.Segment = before.Segment.WithStop(head)
		after.Segment = text.NewSegment(head, after.Segment.Stop)
		return
	}
	before.Segment = before.Segment.WithStop(after.Segment.Stop)
	before.SetSoftLineBreak(after.SoftLineBreak())
	before.SetHardLineBreak(after.HardLineBreak())
	state.parent.RemoveChild(state.parent, after)
}

// settleCut settles the guard's last cut in parent (linkifyBlock.settle).
func settleCut(pc parser.Context, parent ast.Node) {
	if state, _ := pc.Get(linkifyBlockKey).(*linkifyBlock); state != nil && state.parent == parent {
		state.settle()
	}
}

// CloseBlock settles the guard's last cut and links the addresses the transform reads in parent's
// text (linkTextAddresses), unless the write's markdown has passed its element limit, which leaves
// the tree unread.
func (linkifyGuard) CloseBlock(parent ast.Node, block text.Reader, pc parser.Context) {
	settleCut(pc, parent)
	pc.Set(linkifyBlockKey, nil)
	if count := countedElements(pc); count == nil || !count.passed() {
		linkTextAddresses(parent, block.Source())
	}
}

// readLiteralAddress reads the address the tokenizer starts first from where the guard is asked,
// segment's start, up to the next character goldmark asks at, as an autolink, appending the text
// before it; or returns nil where the tokenizer starts none or refuses each it starts.
func readLiteralAddress(parent ast.Node, block text.Reader, pc parser.Context, segment text.Segment) ast.Node {
	source, stop := block.Source(), segment.Stop
	// The asked character is the line's padding, whitespace, where the line has some; otherwise the
	// one at segment's start.
	asked, previous := segment.Start, ' '
	if segment.Padding != 0 {
		asked = -1
	} else {
		previous = previousCharacter(parent, block, segment.Start)
	}
	for at := segment.Start; at < stop; {
		c := source[at]
		if at != asked && (util.IsPunct(c) || util.IsSpace(c)) {
			return nil
		}
		if c == '\\' && at+1 < stop && util.IsPunct(source[at+1]) {
			previous = rune(source[at+1])
			at += 2
			continue
		}
		if length, kind, www := literalAddress(source[at:stop], previous, pc, at); length != 0 {
			if at > segment.Start || segment.Padding != 0 {
				ast.MergeOrAppendTextSegment(parent, segment.WithStop(at))
			}
			block.Advance(segment.Padding + at - segment.Start + length)
			return bareAutolink(kind, www, at, at+length)
		}
		char, width := utf8.DecodeRune(source[at:stop])
		previous = char
		at += width
	}
	return nil
}

// bareAutolink is the autolink of a bare address of kind at source offsets [start, end), whose
// href takes an `http://` before a www URL.
func bareAutolink(kind ast.AutoLinkType, www bool, start, end int) *ast.AutoLink {
	link := ast.NewAutoLink(kind, ast.NewTextSegment(text.NewSegment(start, end)))
	if www {
		link.Protocol = []byte("http")
	}
	return link
}

// literalAddress is the length of the address the tokenizer reads from text's start, the source's
// from offset at to its line's end, after previous, its kind, and whether it is a www URL; or 0
// where it reads none: an email first (literalEmail), then a protocol or www URL (literalURL).
func literalAddress(text []byte, previous rune, pc parser.Context, at int) (int, ast.AutoLinkType, bool) {
	c := text[0]
	if gfmAtext(c) && previousEmail(previous) {
		if length := literalEmail(text); length != 0 {
			return length, ast.AutoLinkEmail, false
		}
	}
	if (c == 'h' || c == 'H') && literalURLFollows(previous, false) || (c == 'w' || c == 'W') && literalURLFollows(previous, true) {
		if length, www := literalURL(text, literalDomainsOf(pc, at, at+len(text))); length != 0 {
			return length, ast.AutoLinkURL, www
		}
	}
	return 0, ast.AutoLinkURL, false
}

// previousCharacter is the character before offset at, where a line's or a table cell's first
// character has a line feed before it, as the tokenizer has a line ending or nothing there.
func previousCharacter(parent ast.Node, block text.Reader, at int) rune {
	if line, _ := block.Position(); line < parent.Lines().Len() && parent.Lines().At(line).Start == at {
		return '\n'
	}
	return block.PrecendingCharacter()
}

// underscoreEmail reads an email starting at a `_`, where the tokenizer tries one before emphasis.
type underscoreEmail struct{}

func (underscoreEmail) Trigger() []byte { return []byte{'_'} }

func (underscoreEmail) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	_, segment := block.PeekLine()
	if pc.IsInLinkLabel() || !previousEmail(previousCharacter(parent, block, segment.Start)) {
		return nil
	}
	length := literalEmail(block.Source()[segment.Start:segment.Stop])
	if length == 0 {
		return nil
	}
	block.Advance(length)
	return ast.NewAutoLink(ast.AutoLinkEmail, ast.NewTextSegment(segment.WithStop(segment.Start+length)))
}

// gfmAtext reports whether c is a character of an email's local part to both readers: to the
// tokenizer, and to the transform's email pattern, `[-.\w+]`.
func gfmAtext(c byte) bool {
	return c == '+' || c == '-' || c == '.' || c == '_' || isASCIIAlphaNumeric(c)
}

// previousEmail reports whether the tokenizer starts an email after previous: neither a character
// of a local part nor `/`.
func previousEmail(previous rune) bool {
	return previous != '/' && (previous >= utf8.RuneSelf || !gfmAtext(byte(previous)))
}

// literalEmail is the length of the email the tokenizer reads from text's start
// (tokenizeEmailAutolink), or 0: a run of local-part characters, an `@`, and a domain of letters,
// digits, `-` and `_` with a dot inside it before a letter or digit, ending in a letter.
func literalEmail(text []byte) int {
	at := 0
	for at < len(text) && gfmAtext(text[at]) {
		at++
	}
	if at == 0 || at == len(text) || text[at] != '@' {
		return 0
	}
	data, dot := false, false
	var last byte
	for at++; at < len(text); at++ {
		c := text[at]
		if c == '.' && at+1 < len(text) && isASCIIAlphaNumeric(text[at+1]) {
			dot = true
		} else if emailDomainCharacter(c) {
			data = true
		} else {
			break
		}
		last = c
	}
	if !data || !dot || !isASCIILetter(last) {
		return 0
	}
	return at
}

// emailDomainCharacter reports whether c is a character of an email's domain other than its dots
// to both readers, `[-\w]`.
func emailDomainCharacter(c byte) bool {
	return c == '-' || c == '_' || isASCIIAlphaNumeric(c)
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

// literalURLPrefix reports whether text opens as a www or http(s) autolink literal does, in any
// case: `www.`, or a protocol of letters ending in a colon.
func literalURLPrefix(text []byte) bool {
	return len(text) >= 4 && bytes.EqualFold(text[:4], []byte("www.")) ||
		len(text) >= 5 && bytes.EqualFold(text[:5], []byte("http:")) ||
		len(text) >= 6 && bytes.EqualFold(text[:6], []byte("https:"))
}

// literalURL is the length in bytes of the www or http(s) autolink literal at the start of text, as
// micromark-extension-gfm-autolink-literal's tokenizer reads one, and whether it is a www literal,
// whose href takes an `http://` before it; or 0 where that tokenizer reads none. text ends where
// its line does. A protocol is `http` or `https` in any case followed by `://` and a character
// neither whitespace, a control character nor punctuation; then come the domain and the path.
func literalURL(text []byte, domains *literalDomains) (length int, www bool) {
	if !literalURLPrefix(text) {
		return 0, false
	}
	www = text[3] == '.'
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
