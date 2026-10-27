package pmdoc

import (
	"bytes"
	"html"
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
// is CloseBlock's (linkTextAddresses), over each run of goldmark's text nodes between other nodes.
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
			link := ast.NewAutoLink(kind, ast.NewTextSegment(text.NewSegment(at, at+length)))
			if www {
				link.Protocol = []byte("http")
			}
			return link
		}
		char, width := utf8.DecodeRune(source[at:stop])
		previous = char
		at += width
	}
	return nil
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

// gfmAtext reports whether c is a character of an email's local part to the tokenizer.
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
		} else if c == '-' || c == '_' || isASCIIAlphaNumeric(c) {
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

// decodedLabelAttr is the attribute holding the decoded text of an autolink the transform made
// (linkTextAddresses), which its source spells with escapes and references; a tokenizer's autolink
// shows its source as written.
const decodedLabelAttr = "pmdocDecodedLabel"

// autolinkText is the text an autolink shows and the href it links to: an email's address
// (emailHref), and a URL's text, `http://` before a www URL's.
func autolinkText(link *ast.AutoLink, source []byte) (label, href string) {
	label = string(link.Label(source))
	if decoded, ok := link.AttributeString(decodedLabelAttr); ok {
		label = decoded.(string)
	}
	switch {
	case link.AutoLinkType == ast.AutoLinkEmail:
		return label, emailHref(label)
	case link.Protocol != nil:
		return label, string(link.Protocol) + "://" + label
	}
	return label, label
}

// emailHref is the href an email address links to: the address itself.
func emailHref(address string) string {
	return address
}

// linkTextAddresses links what mdast-util-gfm-autolink-literal's transform links in parent's
// text: each run of goldmark's text nodes between other nodes, outside a link, an image, a code
// span or an autolink, is one of the engine's text nodes (linkRuns).
func linkTextAddresses(parent ast.Node, source []byte) {
	var run []ast.Node
	for node := parent; node != nil; {
		opaque := false
		switch node.(type) {
		case *ast.Link, *ast.Image, *ast.CodeSpan, *ast.AutoLink:
			opaque = true
		}
		if !opaque && node.FirstChild() != nil {
			run = linkRuns(node, source, run)
			node = node.FirstChild()
			continue
		}
		for node != parent && node.NextSibling() == nil {
			node = node.Parent()
		}
		if node == parent {
			return
		}
		node = node.NextSibling()
	}
}

// linkRuns links the addresses in each run of container's text and string children, which a hard
// break ends, as the engine's break node ends its text; run is scratch space, returned for reuse.
func linkRuns(container ast.Node, source []byte, run []ast.Node) []ast.Node {
	for child := container.FirstChild(); child != nil; {
		run = run[:0]
		for child != nil {
			text, isText := child.(*ast.Text)
			if _, isString := child.(*ast.String); !isText && !isString {
				break
			}
			run = append(run, child)
			child = child.NextSibling()
			if isText && readsAsHardBreak(text, source) {
				break
			}
		}
		if len(run) == 0 {
			child = child.NextSibling()
			continue
		}
		linkRun(run, source)
	}
	return run
}

// readsAsHardBreak reports whether text ends in a line break the engine reads as hard: a backslash,
// or two spaces or more and no tab, as parseInlineMarks reads it.
func readsAsHardBreak(text *ast.Text, source []byte) bool {
	if !text.HardLineBreak() {
		return false
	}
	_, run, backslash := lineSuffix(text, source)
	return backslash || len(run) >= 2 && !strings.Contains(run, "\t")
}

// textPiece is one node of a run, its decoded text starting at start in the run's.
type textPiece struct {
	node  ast.Node
	start int
	value string
}

// foundLink is an address the transform found in a run's decoded text, at [start, end); after is
// where its pattern's match ended, which a URL's trail runs to.
type foundLink struct {
	start, end, after int
	email, www        bool
}

// linkRun links the addresses the transform finds in the decoded text of run, one of the engine's
// text nodes: its URLs (findURLs), then the emails in the text they leave (findEmails), the text
// around each split where the source decodes to its edges (linkFound).
func linkRun(run []ast.Node, source []byte) {
	holds := false
	for _, node := range run {
		holds = holds || mayHoldAddress(rawValue(node, source))
	}
	if !holds {
		return
	}
	pieces := make([]textPiece, len(run))
	var decoded strings.Builder
	for index, node := range run {
		pieces[index] = textPiece{node: node, start: decoded.Len(), value: unescapeMarkdownText(rawValue(node, source))}
		decoded.WriteString(pieces[index].value)
		if text, ok := node.(*ast.Text); ok && (text.SoftLineBreak() || text.HardLineBreak()) {
			decoded.WriteByte('\n')
		}
	}
	value := decoded.String()
	var found []foundLink
	from := 0
	for _, url := range findURLs(value) {
		found = findEmails(found, value, from, url.start)
		found = append(found, url)
		found = findEmails(found, value, url.end, url.after)
		from = url.after
	}
	found = findEmails(found, value, from, len(value))
	if len(found) != 0 {
		linkFound(pieces, found, value, source)
	}
}

// rawValue is node's text as the source spells it: a text's segment, its padding included, or a
// string's value.
func rawValue(node ast.Node, source []byte) []byte {
	if text, ok := node.(*ast.Text); ok {
		return text.Segment.Value(source)
	}
	return node.(*ast.String).Value
}

// mayHoldAddress reports whether raw holds what an address needs once decoded: an `@`, a `:`, a
// `www`, or a character reference, which may spell either.
func mayHoldAddress(raw []byte) bool {
	if bytes.ContainsAny(raw, "@:&") {
		return true
	}
	for at := range len(raw) - 2 {
		if raw[at]|0x20 == 'w' && raw[at+1]|0x20 == 'w' && raw[at+2]|0x20 == 'w' {
			return true
		}
	}
	return false
}

// findURLs is the URLs the transform's URL pattern, `(https?://|www(?=\.))([-.\w]+)([^ \t\r\n]*)`
// in any case, links in value (findUrl): a match after the value's start, whitespace or
// punctuation (transformPrevious), whose domain is a correct one, less its trail (splitURL). A
// refused match is searched again one character after where it began, a taken one after its end.
func findURLs(value string) []foundLink {
	var found []foundLink
	var domain urlDomain
	for at := 0; at < len(value); {
		start, domainStart := nextURLPattern(value, at)
		if start < 0 {
			break
		}
		at = start + 1
		if !transformPrevious(value, start, false) {
			continue
		}
		end := domain.read(value, domainStart)
		if !domain.correct(value, domainStart) {
			continue
		}
		after := end
		for after < len(value) && strings.IndexByte(" \t\r\n", value[after]) < 0 {
			after++
		}
		if end = splitURL(value, domainStart, after); end == domainStart {
			continue
		}
		found = append(found, foundLink{start: start, end: end, after: after, www: value[start]|0x20 == 'w'})
		at = after
	}
	return found
}

// nextURLPattern is where the URL pattern next matches in value from at, and where its domain
// begins (a www URL's `www` among it), or -1.
func nextURLPattern(value string, at int) (start, domain int) {
	for ; at < len(value); at++ {
		switch value[at] | 0x20 {
		case 'h':
			for _, protocol := range []string{"https://", "http://"} {
				if end := at + len(protocol); end < len(value) && strings.EqualFold(value[at:end], protocol) && urlWordCharacter(value[end]) {
					return at, end
				}
			}
		case 'w':
			if at+4 <= len(value) && strings.EqualFold(value[at:at+4], "www.") {
				return at, at
			}
		}
	}
	return -1, -1
}

// urlWordCharacter reports whether c is one of the URL pattern's domain characters, `[-.\w]`.
func urlWordCharacter(c byte) bool {
	return c == '-' || c == '.' || c == '_' || isASCIIAlphaNumeric(c)
}

// urlDomain is the run of the URL pattern's domain characters last read, from where a domain began
// in it to its end, with its last two dots and whether the dot-separated parts after each are
// correct: every domain beginning inside the run ends where it does, so a run is read once.
type urlDomain struct {
	from, end            int
	lastDot, dotBefore   int // -1 where there is none
	lastOK, beforeLastOK bool
}

// read reads the run from start, a domain's start, unless the last run read holds it, and returns
// where the run ends.
func (d *urlDomain) read(value string, start int) int {
	if start >= d.from && start < d.end {
		return d.end
	}
	d.from, d.end, d.lastDot, d.dotBefore = start, start, -1, -1
	for d.end < len(value) && urlWordCharacter(value[d.end]) {
		if value[d.end] == '.' {
			d.dotBefore, d.lastDot = d.lastDot, d.end
		}
		d.end++
	}
	if d.lastDot >= 0 {
		d.lastOK = domainPart(value[d.lastDot+1 : d.end])
	}
	if d.dotBefore >= 0 {
		d.beforeLastOK = domainPart(value[d.dotBefore+1 : d.lastDot])
	}
	return d.end
}

// correct reports whether the domain from start to the run's end is one the transform takes
// (isCorrectDomain): two dot-separated parts or more, neither of its last two holding an
// underscore or lacking a letter or digit, unless it is empty.
func (d *urlDomain) correct(value string, start int) bool {
	switch {
	case d.lastDot < start:
		return false
	case d.dotBefore < start:
		return d.lastOK && domainPart(value[start:d.lastDot])
	}
	return d.lastOK && d.beforeLastOK
}

// domainPart reports whether part is a correct last or next-to-last part of a domain: empty, or
// holding a letter or digit and no underscore.
func domainPart(part string) bool {
	if part == "" {
		return true
	}
	if strings.IndexByte(part, '_') >= 0 {
		return false
	}
	for index := range len(part) {
		if isASCIIAlphaNumeric(part[index]) {
			return true
		}
	}
	return false
}

// splitURL is where the URL whose domain begins at start and whose match ends at after ends
// (splitUrl): before its trail, a run of `!"&'),.:;<>?]}` ending it, but through each `)` of the
// trail that closes a `(` the URL left open.
func splitURL(value string, start, after int) int {
	end := after
	for end > start && strings.IndexByte(`!"&'),.:;<>?]}`, value[end-1]) >= 0 {
		end--
	}
	opening, closing := strings.Count(value[start:end], "("), strings.Count(value[start:end], ")")
	for ; opening > closing; closing++ {
		next := strings.IndexByte(value[end:after], ')')
		if next < 0 {
			break
		}
		end += next + 1
	}
	return end
}

// transformPrevious reports whether the transform takes a match at value[at] for what comes
// before it (previous): nothing, whitespace, or punctuation or a symbol, judged by one UTF-16
// code unit, so a character outside the Basic Multilingual Plane is neither; an email takes no
// `/` before it either.
func transformPrevious(value string, at int, email bool) bool {
	if at == 0 {
		return true
	}
	char, _ := utf8.DecodeLastRuneInString(value[:at])
	return !(email && char == '/') && (literalEnd(char) || literalPunctuation(char))
}

// findEmails is found with the emails the transform's email pattern,
// `([-.\w+]+)@([-\w]+(?:\.[-\w]+)+)` after the text's start, whitespace, punctuation or a symbol,
// links in value[from:to], one of the text nodes the URLs left (findEmail): its previous character
// no `/`, its domain ending in no `-`, digit or `_`. A refused match is searched again one
// character after where it began.
func findEmails(found []foundLink, value string, from, to int) []foundLink {
	var local emailRun
	for at := from; at < to; at++ {
		if !emailCharacter(value[at]) || at > from && !transformPrevious(value, at, true) {
			continue
		}
		end := local.read(value, at, to)
		if end == 0 || strings.IndexByte("-_0123456789", value[end-1]) >= 0 {
			continue
		}
		found = append(found, foundLink{start: at, end: end, after: end, email: true})
		at = end - 1
	}
	return found
}

// emailCharacter reports whether c is one of the email pattern's local-part characters, `[-.\w+]`.
func emailCharacter(c byte) bool {
	return c == '+' || urlWordCharacter(c)
}

// emailRun is the run of local-part characters last read, and the email domain after its `@`:
// every match beginning inside the run reaches the same `@` and domain, so a run is read once.
type emailRun struct {
	from, end, domainEnd int
}

// read is where the email pattern's match beginning at at, inside value[:to], ends, or 0 where it
// does not match: a run of local-part characters to an `@`, then a domain of `[-\w]` parts, two or
// more, separated by single dots.
func (r *emailRun) read(value string, at, to int) int {
	if at >= r.from && at < r.end {
		return r.domainEnd
	}
	r.from, r.end, r.domainEnd = at, at, 0
	for r.end < to && emailCharacter(value[r.end]) {
		r.end++
	}
	if r.end == to || value[r.end] != '@' {
		return 0
	}
	part := func(at int) int {
		for at < to && (value[at] == '-' || value[at] == '_' || isASCIIAlphaNumeric(value[at])) {
			at++
		}
		return at
	}
	end := part(r.end + 1)
	if end == r.end+1 {
		return 0
	}
	parts := 1
	for end+1 < to && value[end] == '.' {
		next := part(end + 1)
		if next == end+1 {
			break
		}
		end, parts = next, parts+1
	}
	if parts > 1 {
		r.domainEnd = end
	}
	return r.domainEnd
}

// linkFound replaces the text of pieces, which decode to value, where each link found stands with
// an autolink holding its decoded text, splitting the text nodes around it at the source offsets
// that decode to its edges. A link whose edge falls inside what one escape or character reference
// decodes to, or that would split a string, which no source spells, is left as text.
func linkFound(pieces []textPiece, found []foundLink, value string, source []byte) {
	type placed struct {
		first, last *ast.Text
		start, end  int // source offsets
		label       string
		email, www  bool
	}
	links := make([]placed, 0, len(found))
	offsets := pieceOffsets{source: source}
	index := 0
	for _, link := range found {
		for index < len(pieces) && pieces[index].start+len(pieces[index].value) <= link.start {
			index++
		}
		first, start, ok := offsets.at(pieces[index], link.start-pieces[index].start)
		last := index
		for pieces[last].start+len(pieces[last].value) < link.end {
			if _, isText := pieces[last].node.(*ast.Text); !isText {
				ok = false
			}
			last++
		}
		lastText, end, endOK := offsets.at(pieces[last], link.end-pieces[last].start)
		if ok && endOK {
			links = append(links, placed{first: first, last: lastText, start: start, end: end, label: value[link.start:link.end], email: link.email, www: link.www})
		}
		index = last
	}
	for _, link := range links {
		parent := link.first.Parent()
		if link.start > link.first.Segment.Start || link.first.Segment.Padding != 0 {
			parent.InsertBefore(parent, link.first, ast.NewTextSegment(link.first.Segment.WithStop(link.start)))
			link.first.Segment = text.NewSegment(link.start, link.first.Segment.Stop)
		}
		kind := ast.AutoLinkURL
		if link.email {
			kind = ast.AutoLinkEmail
		}
		autolink := ast.NewAutoLink(kind, ast.NewTextSegment(text.NewSegment(link.start, link.end)))
		if link.www {
			autolink.Protocol = []byte("http")
		}
		autolink.SetAttributeString(decodedLabelAttr, link.label)
		autolink.SetPos(link.start)
		parent.InsertBefore(parent, link.first, autolink)
		for node := ast.Node(link.first); node != link.last; {
			next := node.NextSibling()
			parent.RemoveChild(parent, node)
			node = next
		}
		link.last.Segment = text.NewSegment(link.end, link.last.Segment.Stop)
		if link.last.Segment.IsEmpty() && !link.last.SoftLineBreak() && !link.last.HardLineBreak() {
			parent.RemoveChild(parent, link.last)
		}
	}
}

// pieceOffsets maps an offset in a piece's decoded text to the source offset that decodes to it,
// keeping the map of the last text it read.
type pieceOffsets struct {
	source  []byte
	node    ast.Node
	decoded []int32 // the offset in the text's raw value each decoded offset begins at, -1 inside a unit
}

// at is the text piece is, and the source offset that decodes to its decoded offset, or false for
// a string or an offset inside what one escape or reference decodes to.
func (o *pieceOffsets) at(piece textPiece, offset int) (*ast.Text, int, bool) {
	text, ok := piece.node.(*ast.Text)
	if !ok {
		return nil, 0, false
	}
	if o.node != piece.node {
		o.node, o.decoded = piece.node, decodedOffsets(text.Segment.Value(o.source))
	}
	if len(o.decoded) != len(piece.value)+1 || o.decoded[offset] < 0 {
		return nil, 0, false
	}
	return text, text.Segment.Start + max(0, int(o.decoded[offset])-text.Segment.Padding), true
}

// decodedOffsets maps each offset of what unescapeMarkdownText decodes raw to, and its end, to the
// offset in raw it begins at, or -1 inside what one escape or character reference decodes to:
// backslash escapes are decoded first, then character references in what they leave, each as
// html.UnescapeString reads it (referenceLength).
func decodedOffsets(raw []byte) []int32 {
	unescaped := make([]byte, 0, len(raw))
	from := make([]int32, 0, len(raw)+1)
	for at := 0; at < len(raw); at++ {
		from = append(from, int32(at))
		if at+1 < len(raw) && raw[at] == '\\' && util.IsPunct(raw[at+1]) {
			at++
		}
		unescaped = append(unescaped, raw[at])
	}
	from = append(from, int32(len(raw)))
	decoded := make([]int32, 0, len(unescaped)+1)
	for at := 0; at < len(unescaped); {
		length := 1
		if unescaped[at] == '&' {
			length = referenceLength(unescaped[at:])
			reference := string(unescaped[at : at+length])
			if character := html.UnescapeString(reference); character != reference {
				decoded = append(decoded, from[at])
				for range len(character) - 1 {
					decoded = append(decoded, -1)
				}
				at += length
				continue
			}
		}
		for end := at + length; at < end; at++ {
			decoded = append(decoded, from[at])
		}
	}
	return append(decoded, from[len(unescaped)])
}

// referenceLength is how much of value, which opens with `&`, html.UnescapeString reads as one
// character reference, whether or not it names one: `&#` and decimal digits, or `&#x` and
// hexadecimal ones, at least one, or `&` and letters and digits, each with a `;` after them if
// there is one; or 1 where there is no such reference.
func referenceLength(value []byte) int {
	at := 1
	if len(value) > 1 && value[1] == '#' {
		if len(value) <= 3 {
			return 1
		}
		at = 2
		hex := value[at]|0x20 == 'x'
		if hex {
			at++
		}
		for at < len(value) && (isASCIIDigit(value[at]) || hex && strings.IndexByte("abcdef", value[at]|0x20) >= 0) {
			at++
		}
		if at < len(value) && value[at] == ';' {
			at++
		}
		if at <= 3 {
			return 1
		}
		return at
	}
	for at < len(value) && isASCIIAlphaNumeric(value[at]) {
		at++
	}
	if at < len(value) && value[at] == ';' {
		at++
	}
	return at
}

func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}
