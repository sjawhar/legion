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

// TestFilterEmitsPlainTextAsItArrives: every delta of plain text is emitted as it stands, in
// order and whole, so the first words reach a reader while the model is still writing.
func TestFilterEmitsPlainTextAsItArrives(t *testing.T) {
	deltas := []string{"Dispatch keeps ", "the broadcast or", "der on the issue."}
	emitted := drive(t, citationsOf(citedIssue), deltas...)
	if got, want := emitted.text(), strings.Join(deltas, ""); got != want {
		t.Errorf("answer = %q, want %q", got, want)
	}
	if got := emitted.count(contracts.GoingEventText); got != len(deltas) {
		t.Errorf("stream holds %d text events, want one for each of the %d deltas: %v", got, len(deltas), emitted.names())
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
				t.Errorf("stream holds %d none events, want none for an answer that linked something", got)
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
	changed := emitted.only(t, contracts.GoingEventChanged)
	if dropped := changed.Data.(ChangedData).Dropped; dropped != 2 {
		t.Errorf("changed dropped = %d, want 2", dropped)
	}
	if got, want := emitted.tail(3), []string{contracts.GoingEventChanged, contracts.GoingEventNone, contracts.GoingEventEnd}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("stream ends %v, want %v: %v", got, want, emitted.names())
	}
	for _, name := range emitted.names()[:len(emitted.events)-3] {
		if name != contracts.GoingEventText {
			t.Errorf("stream holds a %s event before its tail, want cleared text alone: %v", name, emitted.names())
		}
	}
	emitted.terminal(t)
}

// TestFilterEndsNoneWhenNothingWasLinked: an answer that links nothing says none, then ends.
func TestFilterEndsNoneWhenNothingWasLinked(t *testing.T) {
	emitted := drive(t, citationsOf(citedIssue), "Dispatch holds nothing about that.")
	if got, want := emitted.names(), []string{contracts.GoingEventText, contracts.GoingEventNone, contracts.GoingEventEnd}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v, want %v", got, want)
	}
	if got := emitted.terminal(t).Data.(EndData).Items; got != 0 {
		t.Errorf("end items = %d, want 0", got)
	}
}

// TestFilterHoldsALinkSpanSplitAcrossDeltas: the span arrives in pieces no one of which is a
// link, and nothing of it is emitted until it closes - what reaches a reader before that carries
// no bracket at all.
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
				// While the span is still held, a reader has seen no part of it: a bracket in
				// the text would be a link the filter emitted before it knew whether to.
				if held := emitted.text(); filter.state != spanNone && strings.ContainsAny(held, "[]") {
					t.Fatalf("after delta %d the answer is %q, want no held span emitted", i, held)
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

// TestFilterReleasesASpanThatIsNoLink: a span resolved as plain text is emitted as it stands -
// a ] no ( follows, a newline inside the span - and an unterminated span is dropped at Close,
// since it was never cleared.
func TestFilterReleasesASpanThatIsNoLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
	}{
		{"a bracket no target follows", "The order is [not a link] here.", "The order is [not a link] here."},
		{"a newline inside the span", "The order is [not\na link either.", "The order is [not\na link either."},
		{"a newline inside the target", "The order is [a](/issues/\nINFRA-1279) here.", "The order is [a](/issues/\nINFRA-1279) here."},
		{"brackets inside brackets", "The order is [one [two](/issues/INFRA-1279) here.", "The order is [one [two](/issues/INFRA-1279) here."},
		{"a span still held at the end", "The order is [unterminated", "The order is "},
		{"a target still held at the end", "The order is [a](/issues/INFRA-1279", "The order is "},
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

// TestFilterStopsOnAnEmitFailure: the stream's writer has failed, so the filter says nothing
// more and every call after it hands that same error back at once.
func TestFilterStopsOnAnEmitFailure(t *testing.T) {
	emitted := &stream{failAt: 2}
	filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
	if err := filter.Write("the first piece"); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	err := filter.Write("the second piece")
	if !errors.Is(err, emitted.fault) {
		t.Fatalf("Write error = %v, want the writer's own failure %v", err, emitted.fault)
	}
	if err := filter.Write("the third piece"); !errors.Is(err, emitted.fault) {
		t.Errorf("Write after a failure = %v, want %v", err, emitted.fault)
	}
	if err := filter.Close(); !errors.Is(err, emitted.fault) {
		t.Errorf("Close after a failure = %v, want %v", err, emitted.fault)
	}
	if len(emitted.events) != 2 {
		t.Errorf("stream holds %d events, want the two written before the failure: %v", len(emitted.events), emitted.names())
	}
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
