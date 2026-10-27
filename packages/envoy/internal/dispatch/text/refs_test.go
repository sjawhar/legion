package text

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExtractRecognizesDispatchAndServerReferences(t *testing.T) {
	body := `dispatch://LEGION-1 dispatch://LEGION-1/spec dispatch://LEGION-1/artifact/design-md@v3 dispatch://LEGION-1/ask/ask-id dispatch://LEGION-1/comment/comment-id dispatch://LEGION-1/message/message-id https://dispatch.example/issues/LEGION-2 https://dispatch.example/issues/LEGION-2/spec https://dispatch.example/issues/LEGION-2/artifacts/design-md?v=3 https://dispatch.example/issues/LEGION-2/asks/ask-two https://dispatch.example/issues/LEGION-2/comments/comment-two https://dispatch.example/issues/LEGION-2/messages/message-two https://example.com/docs`
	got := Extract(body, "https://dispatch.example")
	want := []Ref{
		{Kind: "issue", IssueKey: "LEGION-1", ID: "LEGION-1"},
		{Kind: "artifact", IssueKey: "LEGION-1", ID: "spec"},
		{Kind: "artifact", IssueKey: "LEGION-1", ID: "design-md"},
		{Kind: "ask", IssueKey: "LEGION-1", ID: "ask-id"},
		{Kind: "comment", IssueKey: "LEGION-1", ID: "comment-id"},
		{Kind: "message", IssueKey: "LEGION-1", ID: "message-id"},
		{Kind: "issue", IssueKey: "LEGION-2", ID: "LEGION-2"},
		{Kind: "artifact", IssueKey: "LEGION-2", ID: "spec"},
		{Kind: "artifact", IssueKey: "LEGION-2", ID: "design-md"},
		{Kind: "ask", IssueKey: "LEGION-2", ID: "ask-two"},
		{Kind: "comment", IssueKey: "LEGION-2", ID: "comment-two"},
		{Kind: "message", IssueKey: "LEGION-2", ID: "message-two"},
		{Kind: "url", ID: "https://example.com/docs"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

func TestExtractPreservesUnrecognizedServerAndExternalURLs(t *testing.T) {
	got := Extract("https://dispatch.example/not-a-route https://other.example/issues/LEGION-1", "https://dispatch.example")
	want := []Ref{
		{Kind: "url", ID: "https://dispatch.example/not-a-route"},
		{Kind: "url", ID: "https://other.example/issues/LEGION-1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

func TestExtractRejectsMalformedDispatchVersion(t *testing.T) {
	got := Extract("dispatch://LEGION-1/artifact/spec@vnot-a-number", "https://dispatch.example")
	if len(got) != 0 {
		t.Fatalf("malformed dispatch version produced references %#v", got)
	}
}

func TestExtractTrimsMarkdownLinkDelimiter(t *testing.T) {
	got := Extract("[ask](https://dispatch.example/issues/LEGION-1/asks/abc)", "https://dispatch.example")
	want := []Ref{{Kind: "ask", IssueKey: "LEGION-1", ID: "abc"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

func TestExtractTrimsParenthesizedDispatchReference(t *testing.T) {
	got := Extract("(dispatch://LEGION-1/spec)", "https://dispatch.example")
	want := []Ref{{Kind: "artifact", IssueKey: "LEGION-1", ID: "spec"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

// Offsets are byte positions of the reference's first character, unmoved by the punctuation and
// closing delimiters the grammar trims, so a caller can map a mention to the block around it.
func TestExtractAtReportsByteOffsets(t *testing.T) {
	body := "# Café\n\nSee (dispatch://LEGION-1/spec). Then [ask](https://dispatch.example/issues/LEGION-1/asks/abc), and https://example.com/docs."
	got := ExtractAt(body, "https://dispatch.example")
	want := []Located{
		{Ref: Ref{Kind: "artifact", IssueKey: "LEGION-1", ID: "spec"}, Offset: len("# Café\n\nSee (")},
		{Ref: Ref{Kind: "ask", IssueKey: "LEGION-1", ID: "abc"}, Offset: len("# Café\n\nSee (dispatch://LEGION-1/spec). Then [ask](")},
		{Ref: Ref{Kind: "url", ID: "https://example.com/docs"}, Offset: len("# Café\n\nSee (dispatch://LEGION-1/spec). Then [ask](https://dispatch.example/issues/LEGION-1/asks/abc), and ")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractAt() = %#v; want %#v", got, want)
	}
}

func TestExtractTerminatesArtifactSlugsAtMarkdownPunctuation(t *testing.T) {
	body := "`dispatch://CORE-2/artifact/diagram-png`` See dispatch://CORE-2/artifact/design-md. " +
		`See dispatch://CORE-2/artifact/quoted-md".`
	want := []Ref{
		{Kind: "artifact", IssueKey: "CORE-2", ID: "diagram-png"},
		{Kind: "artifact", IssueKey: "CORE-2", ID: "design-md"},
		{Kind: "artifact", IssueKey: "CORE-2", ID: "quoted-md"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

// The Go reader's half of the shared text table: each body cites the references its row lists, in
// order of first appearance, and nothing else, so the reference graph and the dashboard composer
// agree on what a text cites. The table is generated from `DISPATCH_TEXT_REFERENCES` in
// `@legion/contracts`, which `packages/contracts/src/dispatch-text-references.test.ts` holds this
// file to; the composer walks the same rows. A row's block reference is written canonically, so
// its fragment is the parsed block id as EncodeBlockID writes it and the parsed pin.
func TestExtractMatchesTheSharedTextTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/dispatch-text-references.json")
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	var rows []struct {
		Body   string   `json:"body"`
		Origin string   `json:"origin"`
		Refs   []string `json:"refs"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the shared text table is empty")
	}

	for _, row := range rows {
		t.Run(row.Body, func(t *testing.T) {
			server := row.Origin
			if server == "" {
				server = "https://dispatch.test"
			}
			want := []Ref{}
			for _, ref := range row.Refs {
				parsed := Extract(ref, server)
				if len(parsed) != 1 || parsed[0].Kind == "url" {
					t.Fatalf("%s is not a reference: %#v", ref, parsed)
				}
				if parsed[0].Kind == "block" && !strings.HasSuffix(ref, blockFragment(parsed[0])) {
					t.Fatalf("%s parsed as %#v, whose fragment is %s", ref, parsed[0], blockFragment(parsed[0]))
				}
				want = append(want, parsed[0])
			}
			got := []Ref{}
			for _, ref := range Extract(row.Body, server) {
				if ref.Kind != "url" && !slices.Contains(got, ref) {
					got = append(got, ref)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Extract() cites %#v; want %#v", got, want)
			}
		})
	}
}

// blockFragment writes a parsed block reference's fragment canonically: `#b-`, the id as
// EncodeBlockID writes it, and `@v<N>` for a pinned one.
func blockFragment(ref Ref) string {
	fragment := "#b-" + EncodeBlockID(ref.Block)
	if ref.Pin != 0 {
		fragment += "@v" + strconv.Itoa(ref.Pin)
	}
	return fragment
}

// Every writer of a `#b-` fragment writes the block id one way, the way the contracts' table
// says; the table is generated from `DISPATCH_BLOCK_ID_ENCODINGS` in `@legion/contracts`, which
// `packages/contracts/src/dispatch-block-ref.test.ts` holds this file to.
func TestEncodeBlockIDMatchesTheSharedTable(t *testing.T) {
	raw, err := os.ReadFile("testdata/dispatch-block-id-encodings.json")
	if err != nil {
		t.Fatalf("read table: %v", err)
	}
	var rows []struct {
		ID      string `json:"id"`
		Encoded string `json:"encoded"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse table: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the shared block id table is empty")
	}
	for _, row := range rows {
		if got := EncodeBlockID(row.ID); got != row.Encoded {
			t.Errorf("EncodeBlockID(%q) = %q; want %q", row.ID, got, row.Encoded)
		}
	}
}

// A dashboard page's `#b-` fragment names the block the page lands on, read with any
// percent-encoding. A pin is written only as a `dispatch://` reference, so a raw `@` in a page's
// fragment names no block; an item in the query or a version page is what it names today.
func TestExtractReadsADashboardPageBlockFragment(t *testing.T) {
	const server = "https://dispatch.test"
	cases := []struct {
		href string
		want Ref
	}{
		{"/issues/CORE-1/spec#b-it%40v2", Ref{Kind: "block", IssueKey: "CORE-1", ID: "spec", Block: "it@v2"}},
		{"/projects/CORE/documents/handbook-md#b-q%3a1", Ref{Kind: "block", Project: "CORE", ID: "handbook-md", Block: "q:1"}},
		{"/issues/CORE-1/spec#b-ship-it@v2", Ref{Kind: "artifact", IssueKey: "CORE-1", ID: "spec"}},
		{"/issues/CORE-1/spec#b-", Ref{Kind: "artifact", IssueKey: "CORE-1", ID: "spec"}},
		{"/issues/CORE-1/spec#b-%0A", Ref{Kind: "artifact", IssueKey: "CORE-1", ID: "spec"}},
		{"/issues/CORE-1/spec?v=2#b-ship-it", Ref{Kind: "artifact", IssueKey: "CORE-1", ID: "spec"}},
		{"/projects/CORE/documents/handbook-md?version=3#b-ship-it", Ref{Kind: "artifact", Project: "CORE", ID: "handbook-md"}},
		{"/issues/CORE-1/spec?comment=c1#b-ship-it", Ref{Kind: "comment", IssueKey: "CORE-1", ID: "c1"}},
	}
	for _, tc := range cases {
		got := Extract(server+tc.href, server)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("Extract(%s) = %#v; want %#v", tc.href, got, tc.want)
		}
	}
}

// A block reference names a block in text only while its target id, `<owner>/<slug>#<block id>`
// as TargetID writes it, holds at most MaxTargetBytes, the id counted in its decoded bytes: one
// byte more and the `dispatch://` text is no reference, and a dashboard page whose fragment names
// that block is its document. The bound is the whole target's, for an issue's document and a
// project's alike. A request naming the block (ParseRef) reads it whatever its length.
func TestABlockReferenceIsBoundedByItsWholeTargetID(t *testing.T) {
	const server = "https://dispatch.test"
	const id, encoded = "é", "%C3%A9"
	for _, document := range []struct {
		owner, reference, page string
		ref                    Ref
	}{
		{"CORE-1", "dispatch://CORE-1/artifact/", server + "/issues/CORE-1/artifacts/", Ref{Kind: "artifact", IssueKey: "CORE-1"}},
		{"CORE", "dispatch://CORE/artifact/", server + "/projects/CORE/documents/", Ref{Kind: "artifact", Project: "CORE"}},
	} {
		atBound := strings.Repeat("a", MaxTargetBytes-len(document.owner+"/#"+id))
		for _, test := range []struct {
			name, slug string
			names      bool
		}{
			{"at the bound", atBound, true},
			{"one byte past it", atBound + "a", false},
		} {
			t.Run(document.owner+" "+test.name, func(t *testing.T) {
				doc := document.ref
				doc.ID = test.slug
				block := doc
				block.Kind, block.Block = "block", id
				want, page := []Ref{}, doc
				if test.names {
					want, page = []Ref{block}, block
				}
				if got := Extract(document.reference+test.slug+"#b-"+encoded, server); !reflect.DeepEqual(got, want) {
					t.Errorf("the dispatch:// reference cites %#v; want %#v", got, want)
				}
				if got := Extract(document.page+test.slug+"#b-"+encoded, server); !reflect.DeepEqual(got, []Ref{page}) {
					t.Errorf("the dashboard page cites %#v; want %#v", got, page)
				}
				for _, request := range []string{document.reference + test.slug + "#b-" + encoded, document.page + test.slug + "#b-" + encoded} {
					if got, ok := ParseRef(request, server); !ok || got != block {
						t.Errorf("ParseRef(%.60s…) = %#v, %t; want the block", request, got, ok)
					}
				}
			})
		}
	}
}

// A block id one byte past MaxBlockIDBytes, decoded, is cited by no text: the `dispatch://` text
// is no reference, and a dashboard page whose fragment names it is its document. A request naming
// the block (ParseRef) reads it whatever the length of its id, by the reference and by the page,
// pinned or not; the index keys no such block, so the read finds no node rather than failing. An
// id at the bound is cited.
func TestABlockIDPastTheBoundIsReadByARequestAndCitedByNoText(t *testing.T) {
	const server = "https://dispatch.test"
	document := Ref{Kind: "artifact", IssueKey: "CORE-1", ID: "spec"}
	// `é` is two bytes, written %C3%A9: the count is of the decoded id's bytes.
	atBound := strings.Repeat("é", MaxBlockIDBytes/2)
	for _, test := range []struct {
		name, id string
		cited    bool
	}{
		{"at the bound", atBound, true},
		{"one byte past it", atBound + "a", false},
		{"3200 bytes", strings.Repeat("a", 3200), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			block := document
			block.Kind, block.Block = "block", test.id
			pinned := block
			pinned.Pin = 2
			reference := "dispatch://CORE-1/spec#b-" + EncodeBlockID(test.id)
			page := server + "/issues/CORE-1/spec#b-" + EncodeBlockID(test.id)
			wantReference, wantPage := []Ref{}, []Ref{document}
			if test.cited {
				wantReference, wantPage = []Ref{block}, []Ref{block}
			}
			if got := Extract(reference, server); !reflect.DeepEqual(got, wantReference) {
				t.Errorf("the dispatch:// text cites %.200v; want %.200v", got, wantReference)
			}
			if got := Extract(page, server); !reflect.DeepEqual(got, wantPage) {
				t.Errorf("the dashboard page cites %.200v; want %.200v", got, wantPage)
			}
			for request, want := range map[string]Ref{reference: block, reference + "@v2": pinned, page: block} {
				if got, ok := ParseRef(request, server); !ok || got != want {
					t.Errorf("ParseRef(%.60s…) = %.200v, %t; want %.200v", request, got, ok, want)
				}
			}
		})
	}
}

// Every reference names its target in text only while the target id TargetID writes for it holds
// at most MaxTargetBytes: an issue's key, a document's `<owner>/<slug>` or
// `agent/<session id>/<slug>`, an item's id (counted decoded) and a component's `<project>/<id>`.
// One character more (a byte, or the two of a decoded `é`) and the `dispatch://` text is no
// reference and the dashboard page is only a URL, whatever part of it is long: the issue number,
// the slug, the session id or the item id, in the page's path or query. A request naming the
// target (ParseRef) reads it whatever its length.
func TestEveryReferenceIsBoundedByItsTargetID(t *testing.T) {
	const server = "https://dispatch.test"
	for _, test := range []struct {
		name string
		// The text before and after the long run in a `dispatch://` reference and on a dashboard
		// page (none when page is empty), and the run's character.
		reference, referenceTail, page, pageTail, char string
		// The target id's bytes besides the run.
		overhead int
		ref      func(run string) Ref
	}{
		{"an issue", "dispatch://CORE-1", "", server + "/issues/CORE-1", "", "0", len("CORE-1"),
			func(run string) Ref { return Ref{Kind: "issue", IssueKey: "CORE-1" + run, ID: "CORE-1" + run} }},
		{"an issue's document", "dispatch://CORE-1/artifact/", "", server + "/issues/CORE-1/artifacts/", "", "a", len("CORE-1/"),
			func(run string) Ref { return Ref{Kind: "artifact", IssueKey: "CORE-1", ID: run} }},
		{"a project's document", "dispatch://CORE/artifact/", "", server + "/projects/CORE/documents/", "", "a", len("CORE/"),
			func(run string) Ref { return Ref{Kind: "artifact", Project: "CORE", ID: run} }},
		{"an agent's upload", "dispatch://agent/", "/artifact/x", server + "/agents/", "/artifacts/x", "a", len("agent//x"),
			func(run string) Ref { return Ref{Kind: "artifact", Session: run, ID: "x"} }},
		{"an issue's comment", "dispatch://CORE-1/comment/", "", server + "/issues/CORE-1/comments/", "", "a", 0,
			func(run string) Ref { return Ref{Kind: "comment", IssueKey: "CORE-1", ID: run} }},
		{"an issue's message", "dispatch://CORE-1/message/", "", server + "/issues/CORE-1/messages/", "", "a", 0,
			func(run string) Ref { return Ref{Kind: "message", IssueKey: "CORE-1", ID: run} }},
		{"an ask on an issue's spec", "dispatch://CORE-1/ask/", "", server + "/issues/CORE-1/spec?ask=", "", "a", 0,
			func(run string) Ref { return Ref{Kind: "ask", IssueKey: "CORE-1", ID: run} }},
		{"a comment on a project's document", "dispatch://CORE/artifact/plan-md/comment/", "", server + "/projects/CORE/documents/plan-md?comment=", "", "a", 0,
			func(run string) Ref { return Ref{Kind: "comment", Project: "CORE", ID: run} }},
		{"an item id counted decoded", "dispatch://CORE-1/comment/", "", server + "/issues/CORE-1/comments/", "", "%C3%A9", 0,
			func(run string) Ref {
				return Ref{Kind: "comment", IssueKey: "CORE-1", ID: strings.ReplaceAll(run, "%C3%A9", "é")}
			}},
		{"a component", "dispatch://CORE/component/", "", "", "", "a", len("CORE/"),
			func(run string) Ref { return Ref{Kind: "component", Project: "CORE", ID: run} }},
	} {
		decoded, _ := decodeItemID(test.char)
		decodedChar := len(decoded)
		atBound := strings.Repeat(test.char, (MaxTargetBytes-test.overhead)/decodedChar)
		if (MaxTargetBytes-test.overhead)%decodedChar != 0 {
			t.Fatalf("%s: a run of %q cannot fill the bound exactly", test.name, test.char)
		}
		past := atBound + test.char
		t.Run(test.name, func(t *testing.T) {
			if got, want := Extract(test.reference+atBound+test.referenceTail, server), []Ref{test.ref(atBound)}; !reflect.DeepEqual(got, want) {
				t.Errorf("the dispatch:// reference at the bound cites %.200v; want %.200v", got, want)
			}
			if got := Extract(test.reference+past+test.referenceTail, server); len(got) != 0 {
				t.Errorf("the dispatch:// reference one byte past the bound cites %.200v; want nothing", got)
			}
			if got, ok := ParseRef(test.reference+past+test.referenceTail, server); !ok || got != test.ref(past) {
				t.Errorf("ParseRef of the dispatch:// reference one byte past the bound = %.200v, %t; want %.200v", got, ok, test.ref(past))
			}
			if test.page == "" {
				return
			}
			if got, want := Extract(test.page+atBound+test.pageTail, server), []Ref{test.ref(atBound)}; !reflect.DeepEqual(got, want) {
				t.Errorf("the dashboard page at the bound cites %.200v; want %.200v", got, want)
			}
			page := test.page + past + test.pageTail
			if got, want := Extract(page, server), []Ref{{Kind: "url", ID: page}}; !reflect.DeepEqual(got, want) {
				t.Errorf("the dashboard page one byte past the bound cites %.200v; want only its URL", got)
			}
			if got, ok := ParseRef(page, server); !ok || got != test.ref(past) {
				t.Errorf("ParseRef of the dashboard page one byte past the bound = %.200v, %t; want %.200v", got, ok, test.ref(past))
			}
		})
	}
}

// A body near the 1 MiB request cap that trails a reference with a run of closing parentheses,
// alone or between the emphasis delimiters the trim also drops, or with a run of square brackets
// after a dashboard URL, still cites that reference, in one pass over the run. The bound is
// generous for a loaded machine; a trim that recounts the parentheses for every character it drops
// takes tens of seconds on these bodies.
func TestExtractTrimsALongClosingRunInLinearTime(t *testing.T) {
	const server = "https://dispatch.example"
	const size = 1 << 20
	issue := []Ref{{Kind: "issue", IssueKey: "CORE-1", ID: "CORE-1"}}
	for _, test := range []struct {
		name string
		body string
	}{
		{"parentheses", "dispatch://CORE-1" + strings.Repeat(")", size)},
		{"parentheses and underscores", "dispatch://CORE-1" + strings.Repeat(")_", size/2)},
		{"parentheses and asterisks after a dashboard URL", server + "/issues/CORE-1" + strings.Repeat(")*", size/2)},
		{"square brackets after a dashboard URL", server + "/issues/CORE-1" + strings.Repeat("]", size)},
	} {
		t.Run(test.name, func(t *testing.T) {
			start := time.Now()
			got := Extract(test.body, server)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("Extract() took %s on a %d-byte body; want one pass", elapsed, len(test.body))
			}
			if !reflect.DeepEqual(got, issue) {
				t.Errorf("Extract() = %#v; want %#v", got, issue)
			}
		})
	}
}

func TestInvalidArtifactReferencesDoNotProduceGraphEdges(t *testing.T) {
	body := "dispatch://CORE-1/artifact/design-notes/extra " +
		"https://dispatch.example/projects/CORE/documents/design%2Fnotes " +
		"https://dispatch.example/projects/CORE/documents/Design-notes"
	want := []Ref{
		{Kind: "url", ID: "https://dispatch.example/projects/CORE/documents/design%2Fnotes"},
		{Kind: "url", ID: "https://dispatch.example/projects/CORE/documents/Design-notes"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want only non-graph URLs", got)
	}
}

func TestExtractParsesProjectDocumentReferences(t *testing.T) {
	body := `dispatch://CORE/artifact/design-notes dispatch://CORE/artifact/design-notes@v2 dispatch://CORE/artifact/design-notes/ask/a1 dispatch://CORE/artifact/design-notes/comment/c1 dispatch://CORE/artifact/design-notes@v2/ask/a3 dispatch://CORE dispatch://CORE/spec https://dispatch.example/projects/CORE/documents/design-notes?version=3 https://dispatch.example/projects/CORE/documents/design-notes?ask=a2`
	want := []Ref{
		{Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "ask", Project: "CORE", ID: "a1"},
		{Kind: "comment", Project: "CORE", ID: "c1"},
		{Kind: "ask", Project: "CORE", ID: "a3"},
		{Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "ask", Project: "CORE", ID: "a2"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

// A component is addressed under its project: dispatch://<PROJECT>/component/<id>, the id in
// the architecture importer's slug charset. Anything else after component/ is not a reference.
func TestExtractParsesComponentReferences(t *testing.T) {
	body := "dispatch://CORE/component/dispatch-server dispatch://CORE/component/Web dispatch://CORE/component/web/extra dispatch://CORE/component/ (dispatch://CORE/component/web)."
	want := []Ref{
		{Kind: "component", Project: "CORE", ID: "dispatch-server"},
		{Kind: "component", Project: "CORE", ID: "web"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

// An artifact an agent's conversation owns is addressed under the session id, as written, with an
// optional version, inside a picture's syntax as anywhere else; its dashboard page names the same
// artifact, and writing the reference back from what was parsed reads the same artifact again. An
// id holding a `/`, `?`, `#` or whitespace, or an empty one, names nothing.
func TestExtractParsesAgentArtifactReferences(t *testing.T) {
	const server = "https://dispatch.example"
	want := Ref{Kind: "artifact", Session: "01a1058e-f14f", ID: "shot-png"}
	for _, body := range []string{
		"dispatch://agent/01a1058e-f14f/artifact/shot-png",
		"dispatch://agent/01a1058e-f14f/artifact/shot-png@v3",
		"![shot.png](dispatch://agent/01a1058e-f14f/artifact/shot-png@v1)",
		server + "/agents/01a1058e-f14f/artifacts/shot-png",
		server + "/agents/01a1058e-f14f/artifacts/shot-png?v=3",
	} {
		got := Extract(body, server)
		if len(got) != 1 || got[0] != want {
			t.Errorf("Extract(%q) = %#v; want %#v", body, got, want)
			continue
		}
		written := "dispatch://agent/" + got[0].Session + "/artifact/" + got[0].ID
		if again := Extract(written, server); len(again) != 1 || again[0] != want {
			t.Errorf("Extract(%q), written back from %q, = %#v; want %#v", written, body, again, want)
		}
	}
	for _, body := range []string{
		"dispatch://agent/a/b/artifact/shot-png",
		"dispatch://agent//artifact/shot-png",
		"dispatch://agent/a?b/artifact/shot-png",
		"dispatch://agent/a#b/artifact/shot-png",
		"dispatch://agent/abc/artifact/Shot-png",
		"dispatch://agent/abc/artifact/shot-png@v0",
		"dispatch://agent/abc/artifact/shot-png/comment/c1",
		"dispatch://agent/abc",
		server + "/agents/a%2Fb/artifacts/shot-png",
		server + "/agents/a%20b/artifacts/shot-png",
		server + "/agents/abc/artifacts/shot-png?v=0",
		server + "/agents/abc/artifacts/shot-png?comment=c1",
	} {
		for _, ref := range Extract(body, server) {
			if ref.Kind != "url" {
				t.Errorf("Extract(%q) cites %#v; want nothing", body, ref)
			}
		}
	}
}
