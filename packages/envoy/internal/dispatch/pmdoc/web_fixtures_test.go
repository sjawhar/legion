package pmdoc

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkWebFixture is the `--check` of pmdoc/gen, for the fixture Go writes rather than the ones
// the browser's engine writes: with it the test compares the committed file with what this
// build makes and fails where they differ, and without it the test writes the file. CI runs the
// check, so a reading of a corpus text that changed and a fixture nobody regenerated is a red
// check rather than a web test quietly asserting against a stale tree.
//
//	go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture
//	go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture -args --check
var checkWebFixture = flag.Bool("check", false, "fail when the committed web display fixture is stale instead of writing it")

// webFixturePath is where the fixture lands: inside the web app, beside the tests that import
// it, so a unit test of a view reads a tree this package made rather than one the browser's own
// parser made of the same text.
var webFixturePath = filepath.Join(
	"..", "..", "..", "..", "dispatch", "web", "src", "features", "refs", "__fixtures__",
	"display-trees.json",
)

// webSnippetSentinels are the private-use characters ts_headline writes around what a search
// matched, which api/search.go hands to DisplaySnippetJSON (markStart, markEnd there). A case
// whose markdown holds them gains a snippet tree with the highlight mark over those runs.
const (
	webSnippetStart = "\uE000"
	webSnippetStop  = "\uE001"
)

// webFixtureCase is one stored text the web app's unit tests draw: its markdown, so a view can
// be handed the source its tree was read from, and the tree in each mode.
type webFixtureCase struct {
	Name     string          `json:"name"`
	Markdown string          `json:"markdown"`
	Latest   json.RawMessage `json:"latest"`
	Version  json.RawMessage `json:"version"`
	// Snippet is the tree of this text read as a search hit's snippet, with the words the
	// sentinels name carrying DisplayHighlightMark; absent for a case holding no sentinel.
	Snippet json.RawMessage `json:"snippet,omitempty"`
}

// webFixtureTexts is the corpus the web app's view tests draw. Each name is what a test looks
// the tree up by, so a name is as much part of the fixture as the markdown under it.
var webFixtureTexts = []struct{ name, markdown string }{
	{"formatted-body", "# Deploy\n\nStop the **deploy** and run `make check` first.\n\n- one item\n- two\n"},
	{"one-paragraph", "Stop the deploy and report back.\n"},
	{"a-reference", "Tracked as dispatch://CORE-1 since Monday.\n"},
	{"a-link", "Read [the design](https://example.com/design) first.\n"},
	{"a-picture", "![shot.png](dispatch://CORE-1/artifact/shot-png@v1)\n"},
	{"a-mermaid-block", "Before.\n\n```mermaid\ngraph TD\n  A --> B\n```\n\nAfter.\n"},
	{"a-task-list", "- [x] done\n- [ ] open\n"},
	{
		"a-long-answer",
		"The deploy is held until the migration lands, and the migration is waiting on a " +
			"review nobody has started. I will pick the review up this afternoon and report " +
			"back once the migration is in and the deploy has run to the end.\n",
	},
	{
		"an-answered-decision",
		":::ask{#q2 urgency=\"blocking\" multiple=\"false\" state=\"resolved\" answered_by=\"alice\"" +
			" answered_at=\"2026-09-12T13:20:00Z\" selected=\"[&#x22;REST&#x22;]\"" +
			" answer=\"Ship REST first.\"}\nWhich transport should we expose?\n\n" +
			"* REST: matches the platform\n:::\n",
	},
	{
		"a-marked-snippet",
		"Stop the " + webSnippetStart + "deploy" + webSnippetStop + " and watch the " +
			webSnippetStart + "port" + webSnippetStop + " it listens on.\n",
	},
	{
		"a-marked-link",
		"Read [the " + webSnippetStart + "design" + webSnippetStop + "](https://example.com/d) first.\n",
	},
	{
		"a-marked-reference",
		"Tracked as " + webSnippetStart + "dispatch://CORE-1" + webSnippetStop + " since Monday.\n",
	},
	{
		"a-marked-link-to-a-reference",
		"See [the " + webSnippetStart + "design" + webSnippetStop + "](dispatch://CORE-1).\n",
	},
	{
		"marks-inside-formatting",
		"**" + webSnippetStart + "Bold" + webSnippetStop + "** " + webSnippetStart + "word" +
			webSnippetStop + ", see `" + webSnippetStart + "code" + webSnippetStop + "` here\n",
	},
	{
		"a-mark-on-one-word-alone",
		"Report the " + webSnippetStart + "port" + webSnippetStop +
			" number to the support team before we export\n",
	},
	{
		"a-marked-word-and-reference",
		"Report the " + webSnippetStart + "port" + webSnippetStop +
			" to support before we export " + webSnippetStart + "dispatch://CORE-1" +
			webSnippetStop + "\n",
	},
	{
		"a-headline-with-references",
		"Blocked on **dispatch://CORE-2/ask/a1**, see `dispatch://CORE-3`\n",
	},
	{"a-headline-of-emoji", "a" + strings.Repeat("😀", 60) + "\n"},
	{"a-tag-shaped-run", "Use Array<string> here\n"},
	{"a-two-block-title", "**Stop** the deploy\n\n1. one\n2. two\n"},
	// The one body every Markdown surface of the web app is tested with
	// (`markdown-surfaces.test.tsx`), as a search hit: a heading, bold, a code span, a link and
	// a `dispatch://` reference, with two of its words matched.
	{
		"surfaces-fixture-marked",
		"## Heading\n\n**" + webSnippetStart + "Bold" + webSnippetStop +
			"** then `code` and a [link](https://example.com/docs) to dispatch://CORE-1.\n\n" +
			"- first\n- " + webSnippetStart + "second" + webSnippetStop,
	},
	{
		"surfaces-picture-marked",
		"The broken " + webSnippetStart + "layout" + webSnippetStop +
			": ![shot.png](dispatch://CORE-1/artifact/shot-png@v1)",
	},
}

// TestWebDisplayFixture writes, or under --check verifies, the display trees the web app's unit
// tests draw. Every tree is this package's own reading of the markdown beside it, in both modes
// (ReadForDisplay), and a snippet's is the reading a search hit gets, highlight marks and all
// (DisplaySnippetJSON), so a view's test draws exactly what the server would have sent it.
//
// Each reading mints its block ids from a counter rather than at random
// (SetBlockIDGenerator), so this writes the same file on every run and --check answers a stale
// corpus rather than the clock. The readings go through ReadForDisplay itself rather than
// DisplayJSON, whose cache would hand back a tree another test of this package read with other
// ids.
func TestWebDisplayFixture(t *testing.T) {
	cases := make([]webFixtureCase, 0, len(webFixtureTexts))
	for _, text := range webFixtureTexts {
		built := webFixtureCase{
			Name:     text.name,
			Markdown: text.markdown,
			Latest:   countedDisplayJSON(t, text.markdown, DisplayLatest),
			Version:  countedDisplayJSON(t, text.markdown, DisplayVersion),
		}
		if strings.Contains(text.markdown, webSnippetStart) {
			built.Snippet = countedSnippetJSON(t, text.markdown)
		}
		cases = append(cases, built)
	}
	encoded, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatalf("encode web display fixture: %v", err)
	}
	encoded = append(encoded, '\n')

	if *checkWebFixture {
		committed, err := os.ReadFile(webFixturePath)
		if err != nil {
			t.Fatalf("read %s: %v (run `go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture`)", webFixturePath, err)
		}
		if string(committed) != string(encoded) {
			t.Fatalf("the committed web display fixture is stale: run `go test ./internal/dispatch/pmdoc/ -run TestWebDisplayFixture`")
		}
		t.Logf("web display fixture up to date (%d texts)", len(cases))
		return
	}
	if err := os.MkdirAll(filepath.Dir(webFixturePath), 0o755); err != nil {
		t.Fatalf("make the fixture's folder: %v", err)
	}
	if err := os.WriteFile(webFixturePath, encoded, 0o644); err != nil {
		t.Fatalf("write %s: %v", webFixturePath, err)
	}
	t.Logf("wrote %d texts to %s", len(cases), webFixturePath)
}

// countingBlockIDs mints `b-000001`, `b-000002`, … from this read's start, and puts the random
// generator back once the read is done.
func countingBlockIDs() func() {
	minted := 0
	SetBlockIDGenerator(func() string {
		minted++
		return fmt.Sprintf("b-%06d", minted)
	})
	return func() { SetBlockIDGenerator(nil) }
}

// countedDisplayJSON is DisplayJSON's reading of markdown with counted block ids and no cache.
func countedDisplayJSON(t *testing.T, markdown string, mode DisplayMode) json.RawMessage {
	t.Helper()
	defer countingBlockIDs()()
	encoded, err := json.Marshal(ReadForDisplay(markdown, mode).toJSON())
	if err != nil {
		t.Fatalf("encode the display tree of %q: %v", markdown, err)
	}
	return encoded
}

// countedSnippetJSON is DisplaySnippetJSON's reading of a search hit's snippet, with counted
// block ids: the balancing, the read and the highlight are all the server's own.
func countedSnippetJSON(t *testing.T, markdown string) json.RawMessage {
	t.Helper()
	defer countingBlockIDs()()
	tree := DisplaySnippetJSON(markdown, webSnippetStart, webSnippetStop)
	if tree == nil {
		t.Fatalf("the snippet of %q read into no tree", markdown)
	}
	return tree
}
