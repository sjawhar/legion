package going

import (
	"strings"
	"unicode"

	"github.com/sjawhar/envoy/internal/contracts"
)

// spanState is where the filter is in a span that may become a markdown link. A span is held from
// its [ until one of these states resolves it, because a link the citation set does not hold is
// dropped whole and a reader must never see half of one.
type spanState int

const (
	// spanNone is cleared text: nothing is held, and every byte up to the next [ is emitted.
	spanNone spanState = iota
	// spanText is inside the link's own text, awaiting the ] that ends it.
	spanText
	// spanGap is the one byte after that ], which decides whether a link closes at all.
	spanGap
	// spanTarget is inside the link's target, awaiting the ) that closes it.
	spanTarget
)

// answerFilter clears one run's answer, delta by delta. It is unexported because the run loop is
// its only driver: everything a reader sees is an Event the filter emitted.
type answerFilter struct {
	citations map[string]Hit
	emit      func(Event) error

	// state, held and targetAt are the held span: the bytes since its [, and where its target
	// begins in them, so resolving it costs no second scan of the span.
	state    spanState
	held     []byte
	targetAt int

	// words and inWord count the cleared text's words the way strings.Fields does, carried
	// across deltas; named holds the href of every hit kept, which is said once and counted by
	// being there; dropped counts the links dropped as outside the citation set.
	words   int
	inWord  bool
	named   map[string]bool
	dropped int

	cut bool
	err error
}

// newAnswerFilter clears the model's text before a reader sees it: a span that may become a
// markdown link is held until it closes, so no link the server would drop is ever emitted.
// citations is keyed by href, the targets the model was given, and is read as it stands at each
// link, so a hit a later search found is citable too.
func newAnswerFilter(citations map[string]Hit, emit func(Event) error) *answerFilter {
	return &answerFilter{citations: citations, emit: emit, named: make(map[string]bool)}
}

// Write takes one content delta, of any size and on any boundary, and emits the text of it that
// is already cleared. A delta that ends mid-word is emitted as it stands, so the first words
// reach a reader while the model is still writing; a delta that ends inside a link span is held,
// because that span may yet be a link no reader may see.
func (f *answerFilter) Write(delta string) error {
	if f.err != nil {
		return f.err
	}
	for delta != "" && !f.cut {
		var err error
		switch f.state {
		case spanNone:
			delta, err = f.plain(delta)
		case spanText:
			delta, err = f.linkText(delta)
		case spanGap:
			delta, err = f.linkGap(delta)
		case spanTarget:
			delta, err = f.linkTarget(delta)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Close emits what the whole answer came to: the links dropped as outside the citation set, the
// absence of any kept link, and the end. A span still held is dropped, never emitted: it was
// never cleared, and it is no link outside the citation set either, so it raises no changed.
func (f *answerFilter) Close() error {
	if f.err != nil {
		return f.err
	}
	if f.dropped > 0 {
		if err := f.send(Changed(f.dropped)); err != nil {
			return err
		}
	}
	if len(f.named) == 0 {
		if err := f.send(None()); err != nil {
			return err
		}
	}
	return f.send(End(f.words, len(f.named)))
}

// plain emits the cleared text up to the next [ and begins holding the span that [ may open.
func (f *answerFilter) plain(delta string) (string, error) {
	open := strings.IndexByte(delta, '[')
	if open < 0 {
		return "", f.text(delta)
	}
	if err := f.text(delta[:open]); err != nil {
		return "", err
	}
	f.held = append(f.held[:0], '[')
	f.state = spanText
	return delta[open+1:], nil
}

// linkText holds the link's own text. A ] moves to the gap, where the next byte decides. A
// newline ends the hold instead: a markdown inline link the model writes never spans lines, and
// a span held across one would hold back every line after a stray [ with no bound at all, which
// is the one thing streaming an answer may not do. A second [ ends it too, since the text before
// that [ was plainly never a link's.
func (f *answerFilter) linkText(delta string) (string, error) {
	at := strings.IndexAny(delta, "]\n[")
	if at < 0 {
		f.held = append(f.held, delta...)
		return "", nil
	}
	switch delta[at] {
	case ']':
		f.held = append(f.held, delta[:at+1]...)
		f.state = spanGap
		return delta[at+1:], nil
	case '\n':
		f.held = append(f.held, delta[:at+1]...)
		return delta[at+1:], f.release()
	default:
		f.held = append(f.held, delta[:at]...)
		if err := f.release(); err != nil {
			return "", err
		}
		f.held = append(f.held[:0], '[')
		f.state = spanText
		return delta[at+1:], nil
	}
}

// linkGap is the one byte after a ]: a ( opens the target, and anything else says the span was
// never a link. That byte is not consumed, because it may itself open the next span.
func (f *answerFilter) linkGap(delta string) (string, error) {
	if delta[0] != '(' {
		return delta, f.release()
	}
	f.held = append(f.held, '(')
	f.targetAt = len(f.held)
	f.state = spanTarget
	return delta[1:], nil
}

// linkTarget holds the link's target: the ) closes the link and resolves it against the
// citations, and a newline ends the hold for the reason linkText gives.
func (f *answerFilter) linkTarget(delta string) (string, error) {
	at := strings.IndexAny(delta, ")\n")
	if at < 0 {
		f.held = append(f.held, delta...)
		return "", nil
	}
	f.held = append(f.held, delta[:at+1]...)
	if delta[at] == '\n' {
		return delta[at+1:], f.release()
	}
	return delta[at+1:], f.resolve()
}

// release emits a held span that cannot be a link as the plain text it turned out to be, and
// stops holding.
func (f *answerFilter) release() error {
	span := string(f.held)
	f.held = f.held[:0]
	f.state = spanNone
	return f.text(span)
}

// resolve decides a closed link. A target the model was given is kept whole, markdown and all,
// since the reader's page renders markdown, and the hit it names is said once. Any other target
// is dropped whole - no byte of it is emitted - and counted for the changed event Close ends
// with. The target is matched exactly as the model wrote it: a target that is not one of the
// hrefs given is dropped, which is the safe side of a link the server would drop.
func (f *answerFilter) resolve() error {
	hit, cited := f.citations[string(f.held[f.targetAt:len(f.held)-1])]
	f.state = spanNone
	if !cited {
		f.held = f.held[:0]
		f.dropped++
		return nil
	}
	span := string(f.held)
	f.held = f.held[:0]
	added, inWord := addedWords(span, f.inWord)
	if f.words+added > contracts.GoingAnswerWordMax {
		// The cut falls inside this link, and half a link is still a link, so the span is
		// dropped whole. That drop is the cut's doing and not the citation set's, so it raises
		// no changed.
		return f.cutAfter("")
	}
	f.words, f.inWord = f.words+added, inWord
	if err := f.send(Text(span)); err != nil {
		return err
	}
	if f.named[hit.Href] {
		return nil
	}
	f.named[hit.Href] = true
	return f.send(Item(hit))
}

// text emits cleared text as the next piece of the answer and counts its words, cutting the
// answer where the word past contracts.GoingAnswerWordMax begins.
func (f *answerFilter) text(cleared string) error {
	if cleared == "" {
		return nil
	}
	words, inWord := f.words, f.inWord
	for at, letter := range cleared {
		space := unicode.IsSpace(letter)
		if !space && !inWord {
			if words == contracts.GoingAnswerWordMax {
				f.words, f.inWord = words, inWord
				return f.cutAfter(cleared[:at])
			}
			words++
		}
		inWord = !space
	}
	f.words, f.inWord = words, inWord
	return f.send(Text(cleared))
}

// cutAfter emits the last cleared text the word cap had room for and cuts the answer there:
// nothing after it is emitted, and the cut says how many words the answer kept.
func (f *answerFilter) cutAfter(kept string) error {
	f.cut = true
	if kept != "" {
		if err := f.send(Text(kept)); err != nil {
			return err
		}
	}
	return f.send(Cut(f.words))
}

// send hands one event to the stream's writer and keeps its failure: the writer has failed, so
// nothing more can be said, and every call after it returns that same error at once.
func (f *answerFilter) send(event Event) error {
	if err := f.emit(event); err != nil {
		f.err = err
		return err
	}
	return nil
}

// addedWords counts the words cleared adds to an answer whose text so far did or did not end
// inside a word, and says whether it ends inside one. It is strings.Fields' own rule - a word
// begins where a non-space follows a space or the start - carried across deltas, because Fields
// over one delta cannot see the word the delta before it ended in.
func addedWords(cleared string, inWord bool) (int, bool) {
	added := 0
	for _, letter := range cleared {
		space := unicode.IsSpace(letter)
		if !space && !inWord {
			added++
		}
		inWord = !space
	}
	return added, inWord
}
