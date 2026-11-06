package going

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/contracts"
)

// heldByteMax bounds the span the filter holds undecided: a [ whose ] never arrives, or a < that
// closes on no >. At the bound the span is decided as it stands - exactly as Close decides it -
// and the byte that opened it joins the word being built as the ordinary text it turned out to
// be and is judged with that word, because no further byte can make that byte safer: escaped,
// it opens no link, image, tag or code span whatever follows it. The bound is what keeps one
// stray [ from stalling the rest of the answer for a reader, and keeps each delta from
// re-scanning text without end. The word the filter is building waits on a bound of its own,
// heldWordMax: the two are different waits.
const heldByteMax = 1024

// heldWordMax bounds the output word the filter builds before it can judge it. At the bound that
// word is dropped where it stands, and the rest of it - every byte up to the whitespace that
// completes it - is dropped with it and counted once, because a word is judged whole or not at
// all: a bound that emitted the half of a word it had would hand a reader a link whenever GFM
// links that half, the escape being no answer to a literal autolink. The bound is what keeps a
// model that writes no whitespace from holding an answer, and the word buffer from growing with
// the answer.
const heldWordMax = 1024

// linkifyMarks are the marks GFM links a word by with no markdown around it at all: a scheme's
// ://, a www. host, and the mailto: and xmpp: schemes, which it links without one. They are held
// as bytes because every word of every answer is read for them.
var linkifyMarks = [][]byte{[]byte("://"), []byte("www."), []byte("mailto:"), []byte("xmpp:")}

// answerFilter clears one run's answer, delta by delta, and emits plain text alone. Two rules
// make that text safe, and they are not the one rule:
//
//   - Every ASCII punctuation byte it emits is backslash-escaped, which closes markdown's own
//     constructs: a link, an image, a raw HTML tag, a code span and a reference definition are
//     syntax a renderer reads before it decodes an escape, and an escaped byte is none of that
//     syntax - not by itself, and not by two pieces of the answer meeting where the filter
//     dropped what stood between them. Link and image syntax is reduced to the text a reader
//     sees of it, and the links of an answer live in its item frames alone, each carrying a
//     cited hit's own href.
//   - The escape closes nothing of GFM's literal autolinks, which are the links a renderer makes
//     out of text with no markdown in it. A renderer decodes \. and \: before the autolink
//     extension reads the text, so www\.evil\.example reaches a reader as a link all the same.
//     What keeps those out is dropping the word a renderer would link - and the word this judges
//     is the output word: the run of non-whitespace bytes a reader will see, joined across every
//     tag the filter dropped and every link it reduced inside it, because what a renderer reads
//     is the text it was given and not the pieces the server built that text out of.
//
// That is why this holds no rule for reference definitions, code spans or bracket precedence:
// escaped, none of them is syntax any more, and CommonMark's precedence no longer decides what
// the server may emit.
//
// The answer still streams, a word at a time: a word is held until whitespace, the bound or
// Close completes it, because the output word is the smallest piece of text the server can judge
// whole, and a span that may be a link is held until the bytes that decide it arrive. It is
// unexported because the run loop is its only driver: everything a reader sees is an Event the
// filter emitted.
type answerFilter struct {
	citations map[string]Hit
	// emit hands one event to the stream's writer. Its failure is handed straight back to the
	// filter's caller and kept nowhere here: the run already holds that the writer failed, and
	// a second copy of one state is a second thing to keep true.
	emit func(Event) error

	// pending is the raw text of a span no reader has seen: the brackets or the tag whose
	// closing bytes have not arrived. Nothing leaves it before it is decided.
	pending []byte
	// word is the output word being built, unescaped and exactly as a reader will read it once
	// the escapes are decoded: what each scanner unit contributes - an ordinary byte, a run of
	// text, the visible text a link or image was reduced to - and nothing for what was dropped.
	// It is what linkifiable is judged over, which is why it is kept raw.
	word []byte
	// wordDropped says the word being built is already dropped - the bound ran out before the
	// whitespace that completes it - so the rest of it is dropped with it and counted once.
	wordDropped bool
	// items are the hits the words not yet flushed cited, held so each item frame follows the
	// text of the link that cited it.
	items []Hit
	// out is the cleared text of the delta being scanned, sent as one text event, so a reader
	// is told a delta's words in one frame rather than one frame a word.
	out []byte

	// wrote says the model produced answer text, which is the one thing none denies; words
	// counts the words a reader was given, one for each word complete emitted; named holds the
	// href of every hit named, which is said once and counted by being there; dropped counts
	// what the server did not pass on as the model wrote it.
	wrote   bool
	words   int
	named   map[string]bool
	dropped int

	// cut says the answer reached contracts.GoingAnswerWordMax, past which nothing is emitted.
	cut bool
}

// newAnswerFilter clears the model's text before a reader sees it: every ASCII punctuation byte
// of it is escaped, every span that may be a link is held until it is resolved, and every word
// a renderer would link on its own is dropped, so the answer's links are the item frames and
// nothing else. citations is keyed by href, the targets the model was given, and is read as it
// stands at each link, so a hit a later search found is citable too.
func newAnswerFilter(citations map[string]Hit, emit func(Event) error) *answerFilter {
	return &answerFilter{citations: citations, emit: emit, named: make(map[string]bool)}
}

// Write takes one content delta, of any size and on any boundary, and emits the text of it that
// is cleared. The word the delta ends in is held until the next whitespace completes it, which
// is what lets every word be judged before a reader sees it; the words before it go out at once,
// so the answer still reaches the reader while the model is writing it.
func (f *answerFilter) Write(delta string) error {
	if delta == "" {
		return nil
	}
	if !f.wrote && strings.TrimSpace(delta) != "" {
		// The model wrote answer text. This is what none denies, and it is read from the
		// model's own delta rather than from the words the filter kept: an answer whose every
		// word the server dropped is an answer the model wrote, and changed, not none, is what
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

// Close decides what the model's last span and last word left held, then says what the whole
// answer came to: what the server changed, the absence of an answer, and the end.
func (f *answerFilter) Close() error {
	if !f.cut {
		if err := f.scan(true); err != nil {
			return err
		}
		// Close completes the last word, as whitespace would: nothing of the answer is held
		// back once the model has stopped writing.
		if err := f.complete(); err != nil {
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
	// server reduced or dropped every word of is still an answer the model wrote, changed says
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
// the first byte it left held. final says no byte is coming after these, so a span waiting on
// one is decided as it stands rather than held for ever; a span still waiting after heldByteMax
// bytes is decided the same way, which is this bound. It is the one scanning loop, run over the
// held text and over the text a link was reduced to alike.
//
// Every unit decided with final set advances past at least the byte at the scanner's hand, so a
// forced unit cannot leave the loop where it was: that is the invariant this loop rests on, and
// the one thing unit must keep.
func (f *answerFilter) decide(b []byte, final bool) (int, error) {
	at := 0
	for at < len(b) && !f.cut {
		next, err := f.unit(b, at, final)
		if err != nil {
			return next, err
		}
		if next == at {
			if len(b)-at < heldByteMax {
				// The span needs a byte that has not arrived: it stays held, whole.
				break
			}
			if next, err = f.unit(b, at, true); err != nil {
				return next, err
			}
		}
		at = next
	}
	return at, nil
}

// unit decides the one thing b begins at i with - a run of whitespace, a link or image's
// brackets, a raw HTML tag, or a run of text - and answers where the next unit begins. Answering
// i itself says the unit is still held, which only a span and the ! that may open an image ever
// do, and only while final is unset.
func (f *answerFilter) unit(b []byte, i int, final bool) (int, error) {
	if size := spaceAt(b, i); size > 0 {
		return f.spaces(b, i, size)
	}
	if !spanStart(b, i) {
		return f.text(b, i, final), nil
	}
	if b[i] == '<' {
		return f.angle(b, i, final)
	}
	return f.bracket(b, i, final)
}

// spaces emits the run of whitespace at i as it stands - whitespace carries no punctuation, so
// escaping leaves it alone - and it is first of all what completes the word before it.
func (f *answerFilter) spaces(b []byte, i, size int) (int, error) {
	end := i + size
	for end < len(b) {
		size := spaceAt(b, end)
		if size == 0 {
			break
		}
		end += size
	}
	if err := f.complete(); err != nil {
		return end, err
	}
	f.cleared(b[i:end])
	return end, nil
}

// text adds the run of non-space bytes at i, up to the span it may run into, to the word being
// built, and answers where that run ends. It judges nothing: the run is a piece of the output
// word and not a word of its own, and only the whole word a reader will see can be judged.
//
// A run ending in the ! that may yet open an image is given back one byte short, because an
// image is one span and the byte that says whether this is one has not arrived; a run that is
// the lone ! is held whole, as a span is.
func (f *answerFilter) text(b []byte, i int, final bool) int {
	end := i + 1
	for end < len(b) && spaceAt(b, end) == 0 && !spanStart(b, end) {
		end++
	}
	if !final && end == len(b) && b[end-1] == '!' {
		end--
		if end == i {
			return i
		}
	}
	f.add(b[i:end])
	return end
}

// add puts what one scanner unit contributes to the output word: the bytes of it a reader will
// see. What the filter dropped - a tag, an uncited link's target, an image - contributes nothing
// at all, which is how the text on the two sides of it meets in the one word a renderer reads.
// A word that reaches heldWordMax is dropped where it stands and goes on being dropped until the
// whitespace that completes it, so no bound ever hands a reader half a word.
func (f *answerFilter) add(text []byte) {
	if f.wordDropped {
		return
	}
	if len(f.word)+len(text) >= heldWordMax {
		f.word, f.wordDropped = f.word[:0], true
		return
	}
	f.word = append(f.word, text...)
}

// complete judges the output word the filter has built - the run of non-whitespace bytes a
// reader will see, joined across every tag dropped and every link reduced inside it - and emits
// it or drops it whole. A word GFM would link on its own is dropped and counted once, however
// many pieces of the model's text it was built from; every other word is counted and escaped,
// and the word past contracts.GoingAnswerWordMax cuts the answer where it begins rather than
// reaching a reader. It is what whitespace, Close and the word's own bound each end a word
// with, and the one place the answer's words are counted: a word is what this emits, so there
// is nothing else to count them from.
func (f *answerFilter) complete() error {
	if len(f.word) == 0 && !f.wordDropped {
		return f.release()
	}
	var err error
	switch {
	case f.wordDropped || linkifiable(f.word):
		f.dropped++
	case f.words == contracts.GoingAnswerWordMax:
		// The cut falls where this word begins, so no byte of it is escaped or emitted and
		// the words before it have gone out already.
		f.cut = true
		if err = f.flush(); err == nil {
			err = f.emit(Cut(f.words))
		}
	default:
		f.words++
		f.cleared(f.word)
	}
	f.word, f.wordDropped = f.word[:0], false
	if err != nil {
		return err
	}
	return f.release()
}

// release emits the item frames the words just flushed cited. An item follows the text of its
// own link, as the model wrote it, so it waits for the word carrying that text to be judged and
// flushed. A word the filter dropped releases its items all the same: the hit was cited, the
// frame carries that hit's own href, and that frame is the answer's only honest link - the one
// thing a reader of a dropped word is still owed.
func (f *answerFilter) release() error {
	if len(f.items) == 0 || len(f.word) > 0 || f.wordDropped {
		return nil
	}
	items := f.items
	f.items = f.items[:0]
	if f.cut {
		// The cap fell inside the word that carried the link's text, so that text was never
		// emitted: a link whose text a reader was not shown cites nothing for them, and the
		// hit is unnamed again so end counts what the reader was actually given.
		for _, hit := range items {
			delete(f.named, hit.Href)
		}
		return nil
	}
	if err := f.flush(); err != nil {
		return err
	}
	for _, hit := range items {
		if err := f.emit(Item(hit)); err != nil {
			return err
		}
	}
	return nil
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
// model wrote as a link reaches the reader as text alone. Either way the text it was reduced to
// joins the word around it, which is the word a renderer will read.
func (f *answerFilter) inline(b []byte, i, open, textEnd int, final bool) (int, error) {
	at := closerAt(b, textEnd+2, ')', 0)
	if at < 0 {
		return f.ordinary(b, i)
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
	f.items = append(f.items, hit)
	// The item waits for the word that carries the link's own text, so a reader reads the
	// answer in the order the model wrote it.
	return at + 1, f.release()
}

// reference reduces a closed [text][ref], ![alt][ref] or [text][] to the text a reader sees of
// it. Which target the ref names is in a definition the server emits as escaped text and never
// resolves, so such a span names no hit at all, and its text is all of it a reader is owed.
func (f *answerFilter) reference(b []byte, i, open, textEnd int, final bool) (int, error) {
	at := closerAt(b, textEnd+2, ']', '[')
	if at < 0 {
		return f.ordinary(b, i)
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
// words rather than its markup. A < that opens none of those is ordinary text. What the tag
// stood between meets in one word, which is judged as the word a renderer reads.
func (f *answerFilter) angle(b []byte, i int, final bool) (int, error) {
	if i+1 == len(b) {
		return f.undecided(b, i, final)
	}
	if !tagStart(b[i+1]) {
		return f.ordinary(b, i)
	}
	at := closerAt(b, i+2, '>', 0)
	if at < 0 {
		return f.ordinary(b, i)
	}
	if at == len(b) {
		return f.undecided(b, i, final)
	}
	return f.drop(at + 1)
}

// inner adds the text a link or image was reduced to, scanned as the answer's own text is: its
// own words are judged and counted there too, and the word it ends in goes on being built by
// whatever follows the link. It is given whole text, so nothing it scans is ever held.
func (f *answerFilter) inner(text []byte) error {
	_, err := f.decide(text, true)
	return err
}

// undecided answers what a span does when the byte that decides it has not arrived: it stays
// held, whole, for the next delta, or - when final says no byte is coming, or the bound in
// decide has run out of patience - it is the ordinary text it begins with. It is the filter's
// one hold on a span, so no scanner here invents a second rule for waiting.
func (f *answerFilter) undecided(b []byte, i int, final bool) (int, error) {
	if !final {
		return i, nil
	}
	return f.ordinary(b, i)
}

// ordinary adds the one byte at i to the word as the plain text the span it opened turned out to
// be, and goes on scanning at the byte after it, so everything that byte held is judged again on
// its own rather than emitted on the strength of the span that failed.
func (f *answerFilter) ordinary(b []byte, i int) (int, error) {
	f.add(b[i : i+1])
	return i + 1, nil
}

// drop counts the span the scanner has just decided not to pass on - a link or image reduced to
// its text, a tag, or an autolink - and answers where scanning goes on after it.
func (f *answerFilter) drop(next int) (int, error) {
	f.dropped++
	return next, nil
}

// cleared escapes text into the buffer the next text event is sent from. It is the one way a
// byte of the model's text reaches a reader, which is what makes the escape whole: there is no
// second path for a piece of text to leave by unescaped. It is given one completed word or one
// run of whitespace, never the half of either, and nothing at all once the answer is cut.
func (f *answerFilter) cleared(text []byte) {
	if f.cut {
		return
	}
	f.out = escape(f.out, text)
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

// escape appends text to dst with a backslash before every ASCII punctuation byte, which is
// every byte CommonMark allows an escape for - & included, since a character reference such as
// www&#46;evil.example decodes to its character before GFM looks for a link in the text. Escaped,
// no byte of the model's text can open a link, an image, a tag, a code span or a definition, and
// no two pieces of the answer can meet in one. It is no answer to a literal autolink, which a
// renderer finds after it has decoded the escapes: what keeps those out is linkifiable, judged
// over the whole output word.
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

// linkifiable reports whether GFM links the output word on its own, with no markdown around it:
// a scheme's ://, a www. host, the mailto: or xmpp: scheme, or an email address. It is judged
// over the word a reader will see, raw and before the escape, because a renderer decodes the
// escapes before the autolink extension reads the text - an escaped www\.evil\.example is a link
// to a reader, which is why such a word is dropped rather than escaped. Each mark is looked for
// anywhere in the word, not at its start, because GFM links it anywhere too: (www.evil.example)
// and a trailing user@host.tld are both links a reader can click.
func linkifiable(word []byte) bool {
	for _, mark := range linkifyMarks {
		if containsFold(word, mark) {
			return true
		}
	}
	return hasEmail(word)
}

// containsFold reports whether b holds mark anywhere, ignoring the case of ASCII letters, since
// WWW. and www. are the one link to a renderer and must be the one word to the server.
func containsFold(b, mark []byte) bool {
	for i := 0; i+len(mark) <= len(b); i++ {
		if bytes.EqualFold(b[i:i+len(mark)], mark) {
			return true
		}
	}
	return false
}

// hasEmail reports whether the word holds an address GFM links on its own: a local part, an @,
// and a host carrying a dot. GFM also refuses an address whose host does not end in a letter;
// the filter drops such a word all the same, because a word it drops costs a reader one word
// and a word it keeps may cost them a link the server never gave the model.
func hasEmail(word []byte) bool {
	for at, letter := range word {
		if letter != '@' || at == 0 || !emailLocal(word[at-1]) {
			continue
		}
		if emailHost(word[at+1:]) {
			return true
		}
	}
	return false
}

// emailLocal reports whether letter may stand in the local part of an address GFM links.
func emailLocal(letter byte) bool {
	return letter == '.' || letter == '-' || letter == '_' || letter == '+' || alphanumeric(letter)
}

// emailHost reports whether b begins with the host of an address GFM links: two or more labels
// of letters, digits, - and _ with a dot between them, which is the ([-\w]+(?:\.[-\w]+)+) of
// mdast-util-gfm-autolink-literal 2.0.1, the rule the app's own remark pipeline links an
// address by. Any label byte may follow the dot, the - and the _ among them. The punctuation
// the host trails off in is none of the host: GFM leaves a sentence's own full stop out of the
// link rather than refusing the link, so a dot one label byte follows makes the address,
// whatever stands after it.
func emailHost(b []byte) bool {
	for at, letter := range b {
		switch {
		case letter == '.':
			if at+1 < len(b) && hostLabel(b[at+1]) {
				return true
			}
		case hostLabel(letter):
		default:
			return false
		}
	}
	return false
}

// hostLabel reports whether letter may stand in a label of a host GFM links: the [-\w] of the
// host rule above, which is a letter, a digit, a - or a _.
func hostLabel(letter byte) bool {
	return letter == '-' || letter == '_' || alphanumeric(letter)
}

// alphanumeric reports whether letter is an ASCII letter or digit, which is what a host's labels
// and an address's local part are made of.
func alphanumeric(letter byte) bool {
	return 'a' <= letter && letter <= 'z' || 'A' <= letter && letter <= 'Z' || '0' <= letter && letter <= '9'
}

// spanStart reports whether b opens at i the one kind of span the filter reads rather than
// escapes byte by byte: a link or image's brackets, or a raw HTML tag's <. It is the one list of
// those bytes, read by the scan that dispatches on them and by the run of text that stops at
// them.
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

// closerAt is the byte that closes a span the filter has already opened: the ) of a link's
// target, the ] of a reference, the > of a tag. It is the one find-the-closer loop the scanners
// share, so each of them honours what CommonMark says closes nothing - a byte behind a backslash
// - rather than keeping a rule of its own. stop is a byte the span may not hold at all, or zero
// where it may hold any; a newline is such a byte for every span, since none of the model's
// spans one. It answers -1 when it met one of those and len(b) when the closer has not arrived.
func closerAt(b []byte, i int, closer, stop byte) int {
	for at := i; at < len(b); at++ {
		switch letter := b[at]; {
		case letter == '\\':
			at++
		case letter == closer:
			return at
		case letter == '\n' || (stop != 0 && letter == stop):
			return -1
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
// there. It reads runes, not bytes, so the words the filter judges are the words strings.Fields
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
