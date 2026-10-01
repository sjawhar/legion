package text

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
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
// file to; the composer walks the same rows.
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
