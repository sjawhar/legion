package going

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/contracts"
)

// heldByteMax bounds the text the filter holds undecided: a token with no space after it, or a
// span whose closing bytes never arrive. At the bound the held text is decided as it stands -
// exactly as Close decides it - and emitted, because no further byte can make it safer: every
// byte the filter emits is escaped, so a token the bound cut in half can no more become a link
// than a whole one can. The bound is what keeps one stray [ from stalling the rest of the answer
// for a reader, and keeps each delta from re-scanning text without end.
const heldByteMax = 1024

// linkifyMarks are the marks GFM links a token by with no markdown around it at all: a scheme's
// ://, a www. host, and the mailto: and xmpp: schemes, which it links without one. They are held
// as bytes because every token of every answer is read for them.
var linkifyMarks = [][]byte{[]byte("://"), []byte("www."), []byte("mailto:"), []byte("xmpp:")}

// answerFilter clears one run's answer, delta by delta, and emits plain text alone: every ASCII
// punctuation byte it emits is backslash-escaped, so nothing a reader's renderer sees can become
// a link, an image, a tag or a code span - not by itself, and not by two pieces of the answer
// meeting where the filter dropped what stood between them. Link and image syntax is reduced to
// the text a reader sees of it, and the links of an answer live in its item frames alone, each
// carrying a cited hit's own href. That is why this holds no rule for reference definitions,
// code spans or bracket precedence: with every byte escaped, none of them can make a link, and
// CommonMark's precedence no longer decides what the server may emit.
//
// The answer still streams: a token is held until a space, a newline, the bound or Close
// completes it, because a token is the smallest piece of text the server can judge whole, and a
// span that may be a link is held until the bytes that decide it arrive. It is unexported
// because the run loop is its only driver: everything a reader sees is an Event the filter
// emitted.
type answerFilter struct {
	citations map[string]Hit
	// emit hands one event to the stream's writer. Its failure is handed straight back to the
	// filter's caller and kept nowhere here: the run already holds that the writer failed, and
	// a second copy of one state is a second thing to keep true.
	emit func(Event) error

	// pending is the text no reader has seen: the token the last delta ended in, and the span
	// whose closing bytes have not arrived. Nothing leaves it before it is cleared.
	pending []byte
	// out is the cleared text of the delta being scanned, sent as one text event, so a reader
	// is told a delta's words in one frame rather than one frame a word.
	out []byte
	// escaped is where cleared escapes one piece of text before it is counted and buffered,
	// kept across pieces so the one escape of every answer allocates once.
	escaped []byte

	// wrote says the model produced answer text, which is the one thing none denies; words and
	// inWord count the cleared text's words the way strings.Fields does, carried across deltas;
	// named holds the href of every hit named, which is said once and counted by being there;
	// dropped counts what the server did not pass on as the model wrote it.
	wrote   bool
	words   int
	inWord  bool
	named   map[string]bool
	dropped int

	// cut says the answer reached contracts.GoingAnswerWordMax, past which nothing is emitted.
	cut bool
}

// newAnswerFilter clears the model's text before a reader sees it: every byte of it is escaped,
// and every span that may be a link is held until it is resolved, so the answer's links are the
// item frames and nothing else. citations is keyed by href, the targets the model was given, and
// is read as it stands at each link, so a hit a later search found is citable too.
func newAnswerFilter(citations map[string]Hit, emit func(Event) error) *answerFilter {
	return &answerFilter{citations: citations, emit: emit, named: make(map[string]bool)}
}

// Write takes one content delta, of any size and on any boundary, and emits the text of it that
// is cleared. The token the delta ends in is held until the next space completes it, which is
// what lets every token be judged before a reader sees it; the text before it goes out at once,
// so the answer still reaches the reader while the model is writing it.
func (f *answerFilter) Write(delta string) error {
	if delta == "" {
		return nil
	}
	if !f.wrote && strings.TrimSpace(delta) != "" {
		// The model wrote answer text. This is what none denies, and it is read from the
		// model's own delta rather than from the words the filter kept: an answer whose every
		// token the server dropped is an answer the model wrote, and changed, not none, is what
		// says what became of it.
		f.wrote = true
	}
	if f.cut {
		return nil
	}
	f.pending = append(f.pending, delta...)
	if err := f.scan(false); err != nil {
		return err
	}
	return f.flush()
}

// Close decides what the model's last token and last span left held, then says what the whole
// answer came to: what the server changed, the absence of an answer, and the end.
func (f *answerFilter) Close() error {
	if !f.cut {
		if err := f.scan(true); err != nil {
			return err
		}
		if err := f.flush(); err != nil {
			return err
		}
	}
	if f.dropped > 0 {
		if err := f.emit(Changed(f.dropped)); err != nil {
			return err
		}
	}
	// none says the model produced no answer text at all, and says nothing else: an answer the
	// server reduced or dropped every token of is still an answer the model wrote, changed says
	// so, and end's items of 0 is what tells the reader it rests on no source of Dispatch's.
	if !f.wrote {
		if err := f.emit(None()); err != nil {
			return err
		}
	}
	return f.emit(End(f.words, len(f.named)))
}

// scan decides every unit the held text now begins with and holds the rest for the next delta.
func (f *answerFilter) scan(final bool) error {
	b := f.pending
	at, err := f.decide(b, final)
	if at > 0 {
		// What is left is held for the next delta, which appends to it. A delta that decided
		// nothing moves nothing.
		f.pending = append(f.pending[:0], b[at:]...)
	}
	return err
}

// decide scans b from its start, deciding every unit it begins with, and answers the index of
// the first byte it left held. final says no byte is coming after these, so a unit waiting on
// one is decided as it stands rather than held for ever; a unit still waiting after heldByteMax
// bytes is decided the same way, which is the filter's one bound. It is the one scanning loop,
// run over the held text and over the text a link was reduced to alike.
func (f *answerFilter) decide(b []byte, final bool) (int, error) {
	at := 0
	for at < len(b) && !f.cut {
		next, err := f.unit(b, at, final)
		if err != nil {
			return next, err
		}
		if next == at {
			if len(b)-at < heldByteMax {
				// The unit needs a byte that has not arrived: it stays held, whole.
				break
			}
			if next, err = f.unit(b, at, true); err != nil {
				return next, err
			}
			if next == at {
				break
			}
		}
		at = next
	}
	return at, nil
}

// unit decides the one thing b begins at i with - a run of whitespace, a link or image's
// brackets, a raw HTML tag, or a token - and answers where the next unit begins. Answering i
// itself says the unit is still held.
func (f *answerFilter) unit(b []byte, i int, final bool) (int, error) {
	if size := spaceAt(b, i); size > 0 {
		return f.spaces(b, i, size)
	}
	if b[i] == '!' && i+1 == len(b) && !final {
		// The ! may yet open an image, which is read as one span, so it is held until the byte
		// that says whether it does arrives.
		return i, nil
	}
	if !spanStart(b, i) {
		return f.token(b, i, final)
	}
	if b[i] == '<' {
		return f.angle(b, i, final)
	}
	return f.bracket(b, i, final)
}

// spaces emits the run of whitespace at i as it stands - whitespace carries no punctuation, so
// escaping leaves it alone - and it is also what completes the token before it.
func (f *answerFilter) spaces(b []byte, i, size int) (int, error) {
	end := i + size
	for end < len(b) {
		size := spaceAt(b, end)
		if size == 0 {
			break
		}
		end += size
	}
	return end, f.cleared(b[i:end])
}

// token emits the run of non-space bytes at i, up to the span it may run into, and drops it
// whole when GFM would link it on its own. The run is judged only once it is whole, because its
// last byte may be what makes it a link, and it is judged raw, before the escape, because a
// renderer decodes the escapes before it looks for a link. A dropped token leaves the whitespace
// on both sides of it standing, so the text around it cannot meet in a new one.
func (f *answerFilter) token(b []byte, i int, final bool) (int, error) {
	end := i + 1
	for end < len(b) && spaceAt(b, end) == 0 && !spanStart(b, end) {
		end++
	}
	if end == len(b) && !final {
		return i, nil
	}
	if linkifiable(b[i:end]) {
		return f.drop(end)
	}
	return end, f.cleared(b[i:end])
}

// bracket decides the span opening at i with [ or ![: an inline link or image, a reference link
// or image, or neither. Neither is ordinary text, which the escape makes safe: a [r]: definition
// of the model's, wherever it stands and whatever quotes it, reaches a reader as the text it was
// written as and gives no reference a target.
func (f *answerFilter) bracket(b []byte, i int, final bool) (int, error) {
	open := i
	if b[i] == '!' {
		open++
	}
	textEnd := linkEnd(b, open)
	if textEnd < 0 {
		// The line ended before the ] did, and no link or image of the model's spans a line.
		return f.ordinary(b, i)
	}
	if textEnd == len(b) || textEnd+1 == len(b) {
		// The ], or the byte after it that decides the span, has not arrived.
		return f.undecided(b, i, final)
	}
	if bytes.IndexByte(b[open+1:textEnd], '`') >= 0 {
		// A code span binds tighter than the brackets around it, so what a renderer makes of
		// such a span is not the link these brackets suggest, and the server will not name an
		// item on the strength of it: every byte goes out as the escaped text it is.
		return f.ordinary(b, i)
	}
	switch b[textEnd+1] {
	case '(':
		return f.inline(b, i, open, textEnd, final)
	case '[':
		return f.reference(b, i, open, textEnd, final)
	}
	return f.ordinary(b, i)
}

// inline reduces a closed [text](target) or ![alt](target) to the text a reader sees of it, and
// says once which hit the model linked. No byte of the target is ever emitted: the link to a hit
// the model was given is the item frame, which carries that hit's own href, and a text frame
// carries plain text. The target is matched against the citation set exactly as the model wrote
// it, which is the safe side of a link the server would not pass on.
//
// A link to a hit the model was given is no change to count: the item frame carries it, so the
// answer still links what it used. Every other link, and every image - whose target a page
// fetches with no click, and which is therefore never a citation - is counted, because what the
// model wrote as a link reaches the reader as text alone.
func (f *answerFilter) inline(b []byte, i, open, textEnd int, final bool) (int, error) {
	at := textEnd + 2
	for at < len(b) && b[at] != ')' {
		if b[at] == '\n' {
			return f.ordinary(b, i)
		}
		at++
	}
	if at == len(b) {
		return f.undecided(b, i, final)
	}
	hit, cited := f.citations[string(b[textEnd+2:at])]
	if err := f.inner(b[open+1 : textEnd]); err != nil {
		return at + 1, err
	}
	if b[i] == '!' || !cited {
		return f.drop(at + 1)
	}
	if f.cut || f.named[hit.Href] {
		return at + 1, nil
	}
	f.named[hit.Href] = true
	// The link's own text goes out before the item that names it, so a reader reads the answer
	// in the order the model wrote it.
	if err := f.flush(); err != nil {
		return at + 1, err
	}
	return at + 1, f.emit(Item(hit))
}

// reference reduces a closed [text][ref], ![alt][ref] or [text][] to the text a reader sees of
// it. Which target the ref names is in a definition the server emits as escaped text and never
// resolves, so such a span names no hit at all, and its text is all of it a reader is owed.
func (f *answerFilter) reference(b []byte, i, open, textEnd int, final bool) (int, error) {
	at := textEnd + 2
	for at < len(b) && b[at] != ']' {
		if b[at] == '[' || b[at] == '\n' {
			return f.ordinary(b, i)
		}
		at++
	}
	if at == len(b) {
		return f.undecided(b, i, final)
	}
	if err := f.inner(b[open+1 : textEnd]); err != nil {
		return at + 1, err
	}
	return f.drop(at + 1)
}

// angle drops the raw HTML tag or autolink opening at i. Escaped, neither could make a link, but
// neither is an answer either: <scheme:...> and <name@host> are the model's own links, a tag is
// whatever HTML it wrote, img and its silent fetch included, and a reader is owed the answer's
// words rather than its markup. A < that opens none of those is ordinary text.
func (f *answerFilter) angle(b []byte, i int, final bool) (int, error) {
	if i+1 == len(b) {
		return f.undecided(b, i, final)
	}
	if !tagStart(b[i+1]) {
		return f.ordinary(b, i)
	}
	at := i + 2
	for at < len(b) && b[at] != '>' {
		if b[at] == '\n' {
			return f.ordinary(b, i)
		}
		at++
	}
	if at == len(b) {
		return f.undecided(b, i, final)
	}
	return f.drop(at + 1)
}

// inner emits the text a link or image was reduced to, scanned as the answer's own text is: a
// token GFM would link on its own is dropped and counted there too, and every byte of it is
// escaped. It is given whole text, so nothing it scans is ever held.
func (f *answerFilter) inner(text []byte) error {
	_, err := f.decide(text, true)
	return err
}

// undecided answers what a span does when the byte that decides it has not arrived: it stays
// held, whole, for the next delta, or - when final says no byte is coming, or the bound in
// decide has run out of patience - it is the ordinary text it begins with. It is the filter's
// one hold, so no scanner here invents a second rule for waiting.
func (f *answerFilter) undecided(b []byte, i int, final bool) (int, error) {
	if !final {
		return i, nil
	}
	return f.ordinary(b, i)
}

// ordinary emits the one byte at i as the plain text the span it opened turned out to be, and
// goes on scanning at the byte after it, so everything that byte held is judged again on its own
// rather than emitted on the strength of the span that failed.
func (f *answerFilter) ordinary(b []byte, i int) (int, error) {
	return i + 1, f.cleared(b[i : i+1])
}

// drop counts one thing the server did not pass on as the model wrote it - a link or image
// reduced to its text, a tag, a token GFM would link on its own - and answers where scanning
// goes on after it. Every count the changed event carries is made here, so the count and the
// rules that raise it cannot drift apart.
func (f *answerFilter) drop(next int) (int, error) {
	f.dropped++
	return next, nil
}

// cleared escapes text, counts its words and buffers it as the next piece of the answer, cutting
// the answer where the word past contracts.GoingAnswerWordMax begins. It is the one way a byte
// of the model's text reaches a reader, which is what makes the escape whole: there is no second
// path for a piece of text to leave by unescaped.
func (f *answerFilter) cleared(text []byte) error {
	if len(text) == 0 {
		return nil
	}
	f.escaped = escape(f.escaped[:0], text)
	words, inWord, at := countWords(f.escaped, f.words, f.inWord)
	f.words, f.inWord = words, inWord
	if at < 0 {
		f.out = append(f.out, f.escaped...)
		return nil
	}
	// The cut falls where the word past the cap begins, which is never inside an escape: a
	// backslash and the byte it escapes hold no space between them.
	f.out = append(f.out, f.escaped[:at]...)
	f.cut = true
	if err := f.flush(); err != nil {
		return err
	}
	return f.emit(Cut(f.words))
}

// flush sends what is cleared as the next piece of the answer. It is one event a delta, not one
// a word, because the frames a reader reads are the deltas the model wrote.
func (f *answerFilter) flush() error {
	if len(f.out) == 0 {
		return nil
	}
	text := string(f.out)
	f.out = f.out[:0]
	return f.emit(Text(text))
}

// countWords counts the words text adds to an answer that has words words and did or did not end
// inside one, and says where the word past contracts.GoingAnswerWordMax begins, or -1 when text
// stays inside the cap. It is strings.Fields' own rule - a word begins where a non-space follows
// a space or the start - carried across deltas, because Fields over one delta cannot see the word
// the delta before it ended in. It counts the escaped text, which is the text a reader is sent,
// and an escape adds no space and so no word. It is the one place the filter walks text for
// words.
func countWords(text []byte, words int, inWord bool) (int, bool, int) {
	for at, letter := range string(text) {
		space := unicode.IsSpace(letter)
		if !space && !inWord {
			if words == contracts.GoingAnswerWordMax {
				return words, inWord, at
			}
			words++
		}
		inWord = !space
	}
	return words, inWord, -1
}

// escape appends text to dst with a backslash before every ASCII punctuation byte, which is
// every byte CommonMark allows an escape for - & included, since a character reference such as
// www&#46;evil.example decodes to its character before GFM looks for a link in the text. Escaped,
// no byte of the model's text can open a link, an image, a tag, a code span or a definition, and
// no two pieces of the answer can meet in one.
func escape(dst, text []byte) []byte {
	for _, letter := range text {
		if asciiPunctuation(letter) {
			dst = append(dst, '\\')
		}
		dst = append(dst, letter)
	}
	return dst
}

// asciiPunctuation reports whether letter is one of the 32 ASCII punctuation bytes CommonMark
// allows a backslash escape for. A backslash before anything else is a backslash the reader
// sees, so the escape is this set and no byte more.
func asciiPunctuation(letter byte) bool {
	return letter >= '!' && letter <= '/' ||
		letter >= ':' && letter <= '@' ||
		letter >= '[' && letter <= '`' ||
		letter >= '{' && letter <= '~'
}

// linkifiable reports whether GFM links the token on its own, with no markdown around it: a
// scheme's ://, a www. host, the mailto: or xmpp: scheme, or an email address. Each mark is
// looked for anywhere in the token, not at its start, because GFM links it anywhere too -
// (www.evil.example) and a trailing user@host.tld are both links a reader can click.
func linkifiable(token []byte) bool {
	for _, mark := range linkifyMarks {
		if containsFold(token, mark) {
			return true
		}
	}
	return hasEmail(token)
}

// containsFold reports whether b holds mark anywhere, ignoring the case of ASCII letters, since
// WWW. and www. are the one link to a renderer and must be the one token to the server.
func containsFold(b, mark []byte) bool {
	for i := 0; i+len(mark) <= len(b); i++ {
		if bytes.EqualFold(b[i:i+len(mark)], mark) {
			return true
		}
	}
	return false
}

// hasEmail reports whether the token holds an address GFM links on its own: a local part, an @,
// and a host carrying a dot. GFM also refuses a host whose last two labels hold a _; the filter
// drops such a token all the same, because a token it drops costs a reader one word and a token
// it keeps may cost them a link the server never gave the model.
func hasEmail(token []byte) bool {
	for at, letter := range token {
		if letter != '@' || at == 0 || !emailLocal(token[at-1]) {
			continue
		}
		if emailHost(token[at+1:]) {
			return true
		}
	}
	return false
}

// emailLocal reports whether letter may stand in the local part of an address GFM links.
func emailLocal(letter byte) bool {
	return letter == '.' || letter == '-' || letter == '_' || letter == '+' || alphanumeric(letter)
}

// emailHost reports whether b begins with the host of an address GFM links: labels of letters,
// digits, - and _, at least one dot between them, and an alphanumeric last byte, because GFM
// leaves the punctuation an address trails off in out of the link.
func emailHost(b []byte) bool {
	dots, last := 0, byte(0)
	for _, letter := range b {
		switch {
		case letter == '.':
			dots++
		case letter == '-' || letter == '_' || alphanumeric(letter):
		default:
			return dots > 0 && alphanumeric(last)
		}
		last = letter
	}
	return dots > 0 && alphanumeric(last)
}

// alphanumeric reports whether letter is an ASCII letter or digit, which is what a host's labels
// and an address's local part are made of.
func alphanumeric(letter byte) bool {
	return 'a' <= letter && letter <= 'z' || 'A' <= letter && letter <= 'Z' || '0' <= letter && letter <= '9'
}

// spanStart reports whether b opens at i the one kind of span the filter reads rather than
// escapes byte by byte: a link or image's brackets, or a raw HTML tag's <. It is the one list of
// those bytes, read by the scan that dispatches on them and by the token that stops at them.
func spanStart(b []byte, i int) bool {
	return b[i] == '[' || b[i] == '<' || (b[i] == '!' && i+1 < len(b) && b[i+1] == '[')
}

// linkEnd is the ] that closes the [ at open, counting the balanced brackets CommonMark allows
// inside a link's text, so [a [b] c](x) and [[x]](x) are read as the one link each renders as.
// It answers -1 when the line ends before the ] does, since no link or image of the model's
// spans a line, and len(b) when the ] has not arrived.
func linkEnd(b []byte, open int) int {
	depth := 0
	for at := open; at < len(b); at++ {
		switch b[at] {
		case '\\':
			// A backslash escapes the byte after it, which then closes nothing.
			at++
		case '\n':
			return -1
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return at
			}
		}
	}
	return len(b)
}

// tagStart reports whether letter opens what < may hold: a scheme or an address, both of which
// begin with a letter, a closing tag's /, or a tag, a comment or a declaration. Anything else
// after a < is arithmetic or prose, and is emitted as the text it is.
func tagStart(letter byte) bool {
	return letter == '/' || letter == '!' || letter == '?' ||
		('a' <= letter && letter <= 'z') || ('A' <= letter && letter <= 'Z')
}

// spaceAt is the size in bytes of the whitespace rune b holds at i, or zero when b holds none
// there. It reads runes, not bytes, so the tokens the filter judges are the words strings.Fields
// counts even where the model writes a space of its own kind.
func spaceAt(b []byte, i int) int {
	if letter := b[i]; letter < utf8.RuneSelf {
		if unicode.IsSpace(rune(letter)) {
			return 1
		}
		return 0
	}
	letter, size := utf8.DecodeRune(b[i:])
	if unicode.IsSpace(letter) {
		return size
	}
	return 0
}
