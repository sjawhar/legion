package text

import (
	"reflect"
	"testing"
)

func TestExtractRecognizesDispatchAndServerReferences(t *testing.T) {
	body := `dispatch://LEGION-1 dispatch://LEGION-1/spec dispatch://LEGION-1/artifact/design-md@v3 dispatch://LEGION-1/ask/ask-id dispatch://LEGION-1/comment/comment-id https://dispatch.example/issues/LEGION-2 https://dispatch.example/issues/LEGION-2/spec https://dispatch.example/issues/LEGION-2/artifacts/design-md?v=3 https://dispatch.example/issues/LEGION-2/asks/ask-two https://dispatch.example/issues/LEGION-2/comments/comment-two https://example.com/docs`
	got := Extract(body, "https://dispatch.example")
	want := []Ref{
		{Kind: "issue", IssueKey: "LEGION-1", ID: "LEGION-1"},
		{Kind: "artifact", IssueKey: "LEGION-1", ID: "spec"},
		{Kind: "artifact", IssueKey: "LEGION-1", ID: "design-md"},
		{Kind: "ask", IssueKey: "LEGION-1", ID: "ask-id"},
		{Kind: "comment", IssueKey: "LEGION-1", ID: "comment-id"},
		{Kind: "issue", IssueKey: "LEGION-2", ID: "LEGION-2"},
		{Kind: "artifact", IssueKey: "LEGION-2", ID: "spec"},
		{Kind: "artifact", IssueKey: "LEGION-2", ID: "design-md"},
		{Kind: "ask", IssueKey: "LEGION-2", ID: "ask-two"},
		{Kind: "comment", IssueKey: "LEGION-2", ID: "comment-two"},
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

func TestExtractParsesProjectDocumentReferences(t *testing.T) {
	body := `dispatch://CORE/artifact/design-notes dispatch://CORE/artifact/design-notes@v2 dispatch://CORE/artifact/design-notes/ask/a1 dispatch://CORE/artifact/design-notes/comment/c1 dispatch://CORE dispatch://CORE/spec https://dispatch.example/projects/CORE/documents/design-notes?version=3 https://dispatch.example/projects/CORE/documents/design-notes?ask=a2`
	want := []Ref{
		{Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "ask", Project: "CORE", ID: "a1"},
		{Kind: "comment", Project: "CORE", ID: "c1"},
		{Kind: "artifact", Project: "CORE", ID: "design-notes"},
		{Kind: "ask", Project: "CORE", ID: "a2"},
	}
	if got := Extract(body, "https://dispatch.example"); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v; want %#v", got, want)
	}
}
