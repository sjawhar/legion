package text

import (
	"reflect"
	"testing"
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

// Markdown's emphasis, strikethrough and code-span delimiters end a reference of every kind, as
// they already ended an artifact slug: `**dispatch://AGENTC-1400**` is the issue a reader sees
// linked, not the unparseable key `AGENTC-1400**`. A backtick ends one outright, so two code spans
// joined by punctuation are two references; `*`, `_` and `~` are dropped only at the end, as
// GFM's autolinks drop them, so a URL keeps an underscore inside it.
func TestExtractEndsReferencesAtMarkdownDelimiters(t *testing.T) {
	body := "Tied to **dispatch://AGENTC-1400** and `dispatch://LEGION-437`. " +
		"_dispatch://CORE-1/spec_, ~~dispatch://CORE-2~~ and __dispatch://CORE-3/ask/a1__. " +
		"(**https://dispatch.example/issues/CORE-4**) `dispatch://CORE-5`/`dispatch://CORE-6` " +
		"*https://example.com/snake_case_path*."
	want := []Ref{
		{Kind: "issue", IssueKey: "AGENTC-1400", ID: "AGENTC-1400"},
		{Kind: "issue", IssueKey: "LEGION-437", ID: "LEGION-437"},
		{Kind: "artifact", IssueKey: "CORE-1", ID: "spec"},
		{Kind: "issue", IssueKey: "CORE-2", ID: "CORE-2"},
		{Kind: "ask", IssueKey: "CORE-3", ID: "a1"},
		{Kind: "issue", IssueKey: "CORE-4", ID: "CORE-4"},
		{Kind: "issue", IssueKey: "CORE-5", ID: "CORE-5"},
		{Kind: "issue", IssueKey: "CORE-6", ID: "CORE-6"},
		{Kind: "url", ID: "https://example.com/snake_case_path"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}

// A document stores `<dispatch://CORE-7/spec>` as a link whose text is its own target, so the text
// ends at the link's `]` and the target is read on its own: both name the spec. Bold link text
// ends the same way.
func TestExtractReadsALinkWhoseTextIsItsTarget(t *testing.T) {
	body := "See [dispatch://CORE-7/spec](dispatch://CORE-7/spec) and [**dispatch://CORE-8**](dispatch://CORE-8)."
	want := []Ref{
		{Kind: "artifact", IssueKey: "CORE-7", ID: "spec"},
		{Kind: "artifact", IssueKey: "CORE-7", ID: "spec"},
		{Kind: "issue", IssueKey: "CORE-8", ID: "CORE-8"},
		{Kind: "issue", IssueKey: "CORE-8", ID: "CORE-8"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
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
