package going

import (
	"bytes"
	"unicode"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/contracts"
)

// schemeSeparator is what a bare URL carries wherever it sits in a word. A renderer links such a
// word with no markdown around it at all, so a word holding it is a link the model wrote without
// writing a link, and the server judges every word for it.
var schemeSeparator = []byte("://")

// answerFilter clears one run's answer, delta by delta, and emits whole words alone: the word a
// delta ends in is held until a space, a newline or Close completes it, because a word is the
// smallest piece of text the server can judge whole, and a span that may become a link is held
// until the bytes that decide it arrive. Every link form markdown knows is judged here and not
// by the model, since an item's own text can tell the model to write one. It is unexported
// because the run loop is its only driver: everything a reader sees is an Event the filter
// emitted.
type answerFilter struct {
	citations map[string]Hit
	// emit hands one event to the stream's writer. Its failure is handed straight back to the
	// filter's caller and kept nowhere here: the run already holds that the writer failed, and
	// a second copy of one state is a second thing to keep true.
	emit func(Event) error

	// pending is the text no reader has seen: the word the last delta ended in, and the span
	// whose closing bytes have not arrived. Nothing leaves it before it is cleared.
	pending []byte
	// out is the cleared text of the delta being scanned, sent as one text event, so a reader
	// is told a delta's words in one frame rather than one frame a word.
	out []byte
	// lineStart says nothing but spaces has followed the last newline, which is what makes a [
	// the opening of a link reference definition rather than ordinary text.
	lineStart bool

	// words and inWord count the cleared text's words the way strings.Fields does, carried
	// across deltas; named holds the href of every hit kept, which is said once and counted by
	// being there; dropped counts the link forms dropped as outside the citation set.
	words   int
	inWord  bool
	named   map[string]bool
	dropped int

	// cut says the answer reached contracts.GoingAnswerWordMax, past which nothing is emitted.
	cut bool
}

// newAnswerFilter clears the model's text before a reader sees it: every span that may become a
// link, and every word that may be one, is held until it is resolved, so no link the server
// would drop is ever emitted. citations is keyed by href, the targets the model was given, and
// is read as it stands at each link, so a hit a later search found is citable too. The answer
// opens a line, which is where a link reference definition may stand.
func newAnswerFilter(citations map[string]Hit, emit func(Event) error) *answerFilter {
	return &answerFilter{citations: citations, emit: emit, named: make(map[string]bool), lineStart: true}
}

// Write takes one content delta, of any size and on any boundary, and emits the words of it that
// are cleared. The word the delta ends in is held until the next space completes it, which is
// what lets every token be judged before a reader sees it; the words before it go out at once,
// so the answer still reaches the reader word by word while the model is writing it.
func (f *answerFilter) Write(delta string) error {
	if f.cut || delta == "" {
		return nil
	}
	f.pending = append(f.pending, delta...)
	if err := f.scan(false); err != nil {
		return err
	}
	return f.flush()
}

// Close decides what the model's last word and last span left held, then says what the whole
// answer came to: the link forms dropped as outside the citation set, the absence of an answer,
// and the end.
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
	// none says the model produced no answer text at all, and says nothing else: an answer that
	// kept no link is still an answer, and end's items of 0 is what tells the reader it rests on
	// no source of Dispatch's.
	if f.words == 0 {
		if err := f.emit(None()); err != nil {
			return err
		}
	}
	return f.emit(End(f.words, len(f.named)))
}

// scan decides every unit the held text now begins with and leaves the rest held. final says no
// byte is coming after these, so a unit still waiting on one is decided as it stands rather than
// held for ever.
func (f *answerFilter) scan(final bool) error {
	b, i := f.pending, 0
	var err error
	for i < len(b) && !f.cut && err == nil {
		var next int
		next, err = f.unit(b, i, final)
		if next == i {
			// The unit needs a byte that has not arrived: it stays held, whole.
			break
		}
		i = next
	}
	if i > 0 {
		// What is left is held for the next delta, which appends to it. A delta that decided
		// nothing moves nothing.
		f.pending = append(f.pending[:0], b[i:]...)
	}
	return err
}

// unit decides the one thing b begins at i with - a run of spaces, a span that may be a link, an
// image, an autolink or a tag, or a word - and answers where the next unit begins. Answering i
// itself says the unit is still held.
func (f *answerFilter) unit(b []byte, i int, final bool) (int, error) {
	if size := spaceAt(b, i); size > 0 {
		return f.spaces(b, i, size)
	}
	switch b[i] {
	case '!':
		if i+1 == len(b) && !final {
			// The ! may yet open an image, which is dropped whole, so it is held until the
			// byte that says whether it does arrives.
			return i, nil
		}
		if i+1 < len(b) && b[i+1] == '[' {
			return f.bracket(b, i, final)
		}
	case '[':
		return f.bracket(b, i, final)
	case '<':
		return f.angle(b, i, final)
	}
	return f.word(b, i, final)
}

// spaces emits the run of whitespace at i, which can hold no link and so is cleared as it
// stands. The run is also what completes the word before it, and a newline in it opens the line
// a link reference definition may stand at.
func (f *answerFilter) spaces(b []byte, i, size int) (int, error) {
	end := i + size
	for end < len(b) {
		size := spaceAt(b, end)
		if size == 0 {
			break
		}
		end += size
	}
	run := b[i:end]
	if err := f.cleared(string(run), false); err != nil {
		return end, err
	}
	if bytes.IndexByte(run, '\n') >= 0 {
		f.lineStart = true
	}
	return end, nil
}

// word emits the run of non-space bytes at i up to the span it may run into, and drops it whole
// when it is a bare URL: a renderer links a word holding a scheme, a mailto: address or a www.
// host with no markdown at all, so such a word is a link the server never gave the model. The
// run is judged only once it is whole, because its last byte may be what makes it one.
func (f *answerFilter) word(b []byte, i int, final bool) (int, error) {
	end := i
	for end < len(b) && spaceAt(b, end) == 0 {
		end++
	}
	if end == len(b) && !final {
		return i, nil
	}
	if isBareURL(b[i:end]) {
		f.dropped++
		f.lineStart = false
		return end, nil
	}
	stop := end
	for at := i; at < end; at++ {
		if b[at] == '[' || b[at] == '<' || (b[at] == '!' && at+1 < len(b) && b[at+1] == '[') {
			stop = at
			break
		}
	}
	f.lineStart = false
	return stop, f.cleared(string(b[i:stop]), false)
}

// bracket decides the span opening at i with [ or ![: an inline link or image, a reference link
// or image, a link reference definition, or none of those. A ] its text never reaches, or a
// newline or a second [ before it, says the span was never any of them, since no such span of
// the model's spans a line.
func (f *answerFilter) bracket(b []byte, i int, final bool) (int, error) {
	image := b[i] == '!'
	at := i + 1
	if image {
		at++
	}
	for at < len(b) && b[at] != ']' {
		if b[at] == '[' || b[at] == '\n' {
			return f.ordinary(b, i)
		}
		at++
	}
	if at == len(b) || at+1 == len(b) {
		// The ] or the byte after it, which is the byte that decides the span, has not arrived.
		if !final {
			return i, nil
		}
		// A [text] nothing follows is ordinary text: with every definition dropped, it renders
		// as the brackets the model wrote and links nothing.
		return f.ordinary(b, i)
	}
	switch b[at+1] {
	case '(':
		return f.inline(b, i, at, image, final)
	case '[':
		return f.reference(b, i, at, final)
	case ':':
		if !image && f.lineStart {
			return f.definition(b, i, final)
		}
	}
	return f.ordinary(b, i)
}

// inline decides a closed [text](target) or ![alt](target). A target the model was given is kept
// whole, markdown and all, since the reader's page renders markdown, and the hit it names is said
// once. Every other target is dropped whole - no byte of it is emitted - and counted for the
// changed event Close ends with; so is every image, whose target a page fetches with no click,
// and which is therefore never a citation. The target is matched exactly as the model wrote it,
// which is the safe side of a link the server would drop.
func (f *answerFilter) inline(b []byte, i, textEnd int, image, final bool) (int, error) {
	at := textEnd + 2
	for at < len(b) && b[at] != ')' {
		if b[at] == '\n' {
			return f.ordinary(b, i)
		}
		at++
	}
	if at == len(b) {
		if !final {
			return i, nil
		}
		return f.ordinary(b, i)
	}
	f.lineStart = false
	hit, cited := f.citations[string(b[textEnd+2:at])]
	if image || !cited {
		f.dropped++
		return at + 1, nil
	}
	if err := f.cleared(string(b[i:at+1]), true); err != nil {
		return at + 1, err
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

// reference drops a closed [text][ref], ![alt][ref] or [text][]. Which target it names is in a
// definition the filter has not read and may never see, so no such span can be resolved against
// the citations at all, and a link the server cannot clear is a link it drops.
func (f *answerFilter) reference(b []byte, i, textEnd int, final bool) (int, error) {
	at := textEnd + 2
	for at < len(b) && b[at] != ']' {
		if b[at] == '[' || b[at] == '\n' {
			return f.ordinary(b, i)
		}
		at++
	}
	if at == len(b) {
		if !final {
			return i, nil
		}
		return f.ordinary(b, i)
	}
	f.dropped++
	f.lineStart = false
	return at + 1, nil
}

// definition drops a link reference definition to the end of its line. The definition is what
// gives every reference use its target, including a use the model wrote before it, and its own
// target is one the server never gave the model; the newline is left to stand, since it ends a
// line of the answer the reader keeps.
func (f *answerFilter) definition(b []byte, i int, final bool) (int, error) {
	end := bytes.IndexByte(b[i:], '\n')
	if end < 0 {
		if !final {
			return i, nil
		}
		end = len(b) - i
	}
	f.dropped++
	f.lineStart = false
	return i + end, nil
}

// angle drops the autolink or the raw HTML tag opening at i: <scheme:...> and <name@host> are
// links a renderer makes of the angle brackets alone, and a tag is whatever HTML the model
// wrote, img and its silent fetch included. A < that opens none of those - one that begins no
// scheme, address or tag name - is ordinary text and is emitted.
func (f *answerFilter) angle(b []byte, i int, final bool) (int, error) {
	if i+1 == len(b) {
		if !final {
			return i, nil
		}
		return f.ordinary(b, i)
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
		if !final {
			return i, nil
		}
		return f.ordinary(b, i)
	}
	f.dropped++
	f.lineStart = false
	return at + 1, nil
}

// ordinary emits the one byte at i as the plain text the span it opened turned out to be, and
// goes on scanning at the byte after it, so everything that byte held is judged again on its own
// rather than emitted on the strength of the span that failed.
func (f *answerFilter) ordinary(b []byte, i int) (int, error) {
	f.lineStart = false
	return i + 1, f.cleared(string(b[i:i+1]), false)
}

// cleared buffers text the filter has cleared and counts its words, cutting the answer where the
// word past contracts.GoingAnswerWordMax begins. whole says the text is one span that may not be
// cut in half, since half a link is still a link: the cut drops it instead, and that drop is the
// cut's doing and not the citation set's, so it raises no changed.
func (f *answerFilter) cleared(text string, whole bool) error {
	if text == "" {
		return nil
	}
	words, inWord, at := countWords(text, f.words, f.inWord)
	if at >= 0 {
		if !whole {
			f.out = append(f.out, text[:at]...)
			f.words, f.inWord = words, inWord
		}
		f.cut = true
		if err := f.flush(); err != nil {
			return err
		}
		return f.emit(Cut(f.words))
	}
	f.words, f.inWord = words, inWord
	f.out = append(f.out, text...)
	return nil
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
// the delta before it ended in. It is the one place the filter walks text for words.
func countWords(text string, words int, inWord bool) (int, bool, int) {
	for at, letter := range text {
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

// isBareURL reports whether the word is a link a renderer makes of plain text: a scheme's :// in
// it anywhere, or a mailto: address or a www. host it begins with, in whatever case the model
// wrote them.
func isBareURL(word []byte) bool {
	return bytes.Contains(word, schemeSeparator) || hasFoldPrefix(word, "mailto:") || hasFoldPrefix(word, "www.")
}

// hasFoldPrefix reports whether b begins with prefix, ignoring the case of ASCII letters, since
// WWW. and www. are the one link to a renderer and must be the one word to the server.
func hasFoldPrefix(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := range len(prefix) {
		letter := b[i]
		if 'A' <= letter && letter <= 'Z' {
			letter += 'a' - 'A'
		}
		if letter != prefix[i] {
			return false
		}
	}
	return true
}

// tagStart reports whether letter opens what < may hold: a scheme or an address, both of which
// begin with a letter, a closing tag's /, or a tag, a comment or a declaration. Anything else
// after a < is arithmetic or prose, and is emitted as the text it is.
func tagStart(letter byte) bool {
	return letter == '/' || letter == '!' || letter == '?' ||
		('a' <= letter && letter <= 'z') || ('A' <= letter && letter <= 'Z')
}

// spaceAt is the size in bytes of the whitespace rune b holds at i, or zero when b holds none
// there. It reads runes, not bytes, so the words the filter emits are the words strings.Fields
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
