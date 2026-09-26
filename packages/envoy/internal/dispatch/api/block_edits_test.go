package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The ids GET /blocks lists address delete and move; the edits land through the same
// error mapping every other operation uses.
func TestDocumentEditsAddressBlocksByID(t *testing.T) {
	handler := newTestHandler(t)
	const ask = ":::ask{#decision urgency=\"high\" multiple=\"false\" state=\"open\"}\nWhich transport?\n:::\n"
	issue := createInteractionIssue(t, handler, "TEST", "Block edits", "# Title\n\n"+ask+"\nContext ends with no drift.\n\n1. only\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var listID string
	for _, block := range blocks {
		if block.Type == "ordered_list" {
			listID = block.ID
		}
	}
	if listID == "" {
		t.Fatalf("blocks = %#v, want an ordered_list", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "move", "block": "decision", "after": "no drift."},
			{"op": "delete", "block": listID},
		},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":2`) {
		t.Fatalf("block edits: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	if want := "# Title\n\nContext ends with no drift.\n\n" + ask; text.Markdown != want {
		t.Fatalf("after block edits = %q, want %q", text.Markdown, want)
	}

	for _, test := range []struct {
		name   string
		op     map[string]any
		status int
		code   string
		detail string
	}{
		{name: "anchor inside the moved block", op: map[string]any{"op": "move", "block": "decision", "after": "transport"}, status: http.StatusBadRequest, code: "INVALID_OP", detail: `field \"after\"`},
		{name: "unknown block", op: map[string]any{"op": "delete", "block": "missing"}, status: http.StatusNotFound, code: "TARGET_NOT_FOUND", detail: `block \"missing\"`},
		{name: "move without an anchor", op: map[string]any{"op": "move", "block": "decision"}, status: http.StatusBadRequest, code: "INVALID_OP", detail: "after or before"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]any{test.op}}, "alice")
			body := response.Body.String()
			if response.Code != test.status || !strings.Contains(body, `"code":"`+test.code+`"`) || !strings.Contains(body, test.detail) {
				t.Fatalf("%s: status=%d body=%s", test.name, response.Code, body)
			}
		})
	}
}

// A replace is refused only when it makes the block it lands in unreadable. A document that
// already holds a block the parser refuses, as a delete can leave one, still takes replaces
// elsewhere and inside that block.
func TestDocumentEditsReplaceBesideAnUnreadableBlockIsAccepted(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Unreadable block", "Intro.\n\nfoo <div>x</div> bar\n\nOther text.\n")
	edit := func(op map[string]any) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]any{op},
		}, "alice")
	}
	if deleted := edit(map[string]any{"op": "delete", "find": "foo "}); deleted.Code != http.StatusOK {
		t.Fatalf("delete: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	for _, op := range []map[string]any{
		{"op": "replace", "find": "Other text.", "with": "Changed."},
		{"op": "replace", "find": "bar", "with": "baz"},
	} {
		if replaced := edit(op); replaced.Code != http.StatusOK {
			t.Fatalf("replace %v: status=%d body=%s", op, replaced.Code, replaced.Body.String())
		}
	}
}

// A replace that leaves its block unreadable is refused with advice for its cause: HTML that
// opens a block keeps to a line; an emptied paragraph is removed by deleting its text where that
// delete removes it, and by deleting the block that holds it where the delete would be refused -
// a typed block, a footnote definition, a list item holding more than the paragraph. No refusal
// names a Go type.
func TestDocumentEditsRefuseAnUnreadableReplaceWithAdviceForItsCause(t *testing.T) {
	handler := newTestHandler(t)
	for index, test := range []struct {
		name, spec, with string
		advice, absent   []string
	}{
		{"block HTML", "Intro.\n\nBody.\n", "<div>x</div>", []string{"HTML", "inside a line"}, nil},
		{"emptied list item", "- Body.\n- two\n", "", []string{"delete", "find"}, []string{"HTML"}},
		{"emptied blockquote", "Intro.\n\n> Body.\n", "", []string{"delete", "find"}, []string{"HTML"}},
		{"emptied typed block", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n", "", []string{"delete {block:"}, []string{"HTML"}},
		{"emptied footnote", "x[^1]\n\n[^1]: Body.\n", "", []string{"delete {block:"}, []string{"HTML"}},
		{"emptied list item holding more", "- Body.\n\n  ```\n  code\n  ```\n", "", []string{"delete {block:"}, []string{"find", "HTML"}},
		{"emptied list, a callout's only block", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n- Body.\n:::\n", "", []string{"delete {block:"}, []string{"HTML"}},
		{"emptied blockquote, a callout's only block", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n> Body.\n:::\n", "", []string{"delete {block:"}, []string{"HTML"}},
		{"emptied first paragraph of a list item", "- Body.\n\n  more\n- two\n", "", []string{"remove the paragraph with delete {block:"}, []string{"holding it", "HTML"}},
		{"emptied only option of an ask", "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- Body.\n:::\n", "", []string{"delete"}, []string{"HTML"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "T"+string(rune('A'+index)), test.name, test.spec)
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": test.with}},
			}, "alice")
			body := response.Body.String()
			if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) {
				t.Fatalf("replace: status=%d body=%s", response.Code, body)
			}
			for _, want := range test.advice {
				if !strings.Contains(body, want) {
					t.Fatalf("refusal %s lacks %q", body, want)
				}
			}
			for _, unwanted := range append(test.absent, "*ast.") {
				if strings.Contains(body, unwanted) {
					t.Fatalf("refusal %s says %q", body, unwanted)
				}
			}
			// The advice is the caller's next call, so it has to be one the route accepts.
			var advised map[string]any
			if match := regexp.MustCompile(`delete \{block:\\"([^\\"]+)\\"\}`).FindStringSubmatch(body); match != nil {
				advised = map[string]any{"op": "delete", "block": match[1]}
			} else if strings.Contains(body, "delete and find") {
				advised = map[string]any{"op": "delete", "find": "Body."}
			}
			if advised == nil {
				return
			}
			followed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{advised},
			}, "alice")
			if followed.Code != http.StatusOK {
				t.Fatalf("the advised %v: status=%d body=%s", advised, followed.Code, followed.Body.String())
			}
		})
	}
}

// The browser editor's parser, like this one, ends a typed block at a line of three or more colons
// indented less than four columns from the typed block's own prefix, even inside fenced code the
// typed block holds; list markers add their width to a code line's indentation, a tab counts four,
// and a line in a blockquote closes nothing. A replace or a suggestion that writes such a line into
// code inside a callout would store a document that reads back with the callout cut short, so it
// is refused and nothing is written. The shapes the engine keeps are stored as sent; the fixture
// generator's typed-fence-lines.json holds the engine's reading of each shape
// (pmdoc.TestTypedFenceLineInCodeAgreesWithTheEngine).
func TestDocumentEditsRefuseCodeThatWouldEndItsTypedBlock(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	const (
		direct   = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n```\nBody.\n```\n:::\n\nAfter.\n"
		listItem = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n- item\n  ```\n  Body.\n  ```\n:::\n\nAfter.\n"
		quoted   = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n> ```\n> Body.\n> ```\n:::\n\nAfter.\n"
		outside  = "Intro.\n\n```\nBody.\n```\n\nAfter.\n"
		// A callout inside a blockquote or a list item starts two columns in, where a tab in its
		// code advances only two columns.
		calloutInQuote = "Intro.\n\n> :::callout{#c1 kind=\"note\" title=\"T\"}\n> ```\n> Body.\n> ```\n> :::\n\nAfter.\n"
		calloutInItem  = "Intro.\n\n- item\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  ```\n  Body.\n  ```\n  :::\n\nAfter.\n"
	)
	text := func(artifactID string) string {
		markdown, err := documentService.Text(context.Background(), artifactID)
		if err != nil {
			t.Fatal(err)
		}
		return markdown
	}
	for index, test := range []struct{ name, spec, with string }{
		{"a closing line", direct, "a\n:::\nb"},
		{"with a trailing space", direct, "a\n::: \nb"},
		{"opening the code", direct, ":::\nb"},
		{"ending the code", direct, "a\n:::"},
		{"indented two spaces", direct, "a\n  :::\nb"},
		{"indented three spaces", direct, "a\n   :::\nb"},
		{"four colons", direct, "a\n::::\nb"},
		{"in a list item's code", listItem, "a\n:::\nb"},
		{"indented one space in a list item's code", listItem, "a\n :::\nb"},
		{"a tab in code in a callout in a blockquote", calloutInQuote, "a\n\t:::\nb"},
		{"a tab in code in a callout in a list item", calloutInItem, "a\n\t:::\nb"},
		{"a space and a tab in code in a callout in a list item", calloutInItem, "a\n \t:::\nb"},
		{"a line of four colons ending in CR LF", direct, "a\r\n::::\r\nb"},
		{"a line ending in CR LF in a callout in a list item", calloutInItem, "a\r\n :::\r\nb"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "E"+string(rune('A'+index)), "closer in code", test.spec)
			before := text(issue.PrimaryArtifactID)
			edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": test.with}},
			}, "alice")
			if body := edited.Body.String(); edited.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) || !strings.Contains(body, "indent that line four or more spaces, or move") || !strings.Contains(body, "out of the callout") {
				t.Fatalf("replace: status=%d body=%s", edited.Code, body)
			}
			if after := text(issue.PrimaryArtifactID); after != before {
				t.Fatalf("after a refused replace = %q, want it unchanged, %q", after, before)
			}
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": "Body."},
				"suggestion": map[string]string{"replace_with": test.with}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)
			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
			if body := accepted.Body.String(); accepted.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) || !strings.Contains(body, "indent that line four or more spaces, or move") {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, body)
			}
			if after := text(issue.PrimaryArtifactID); after != before {
				t.Fatalf("after a refused accept = %q, want it unchanged, %q", after, before)
			}
		})
	}
	for index, test := range []struct{ name, spec, with string }{
		{"indented four spaces", direct, "a\n    :::\nb"},
		{"indented a tab", direct, "a\n\t:::\nb"},
		{"a no-break space after the colons in a list item's code", listItem, "a\n:::\u00a0\nb"},
		{"indented two spaces in a list item's code", listItem, "a\n  :::\nb"},
		{"in a blockquote's code", quoted, "a\n:::\nb"},
		{"in code outside a typed block", outside, "a\n:::\nb"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "F"+string(rune('A'+index)), test.name, test.spec)
			edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": test.with}},
			}, "alice")
			if edited.Code != http.StatusOK {
				t.Fatalf("replace: status=%d body=%s", edited.Code, edited.Body.String())
			}
			stored := text(issue.PrimaryArtifactID)
			back, err := pmdoc.Parse(stored)
			if err != nil || len(back.Children) != 3 {
				t.Fatalf("%q does not read back as three blocks (%v)", stored, err)
			}
			var code string
			pmdoc.Walk(back, func(node *pmdoc.Node) bool {
				if node.Type == "code_block" && code == "" {
					code = node.Children[0].Text
				}
				return true
			})
			if code != test.with {
				t.Fatalf("%q reads back holding the code %q, want %q", stored, code, test.with)
			}
		})
	}
}

// Accepting a suggestion writes its replacement through the same shape checks a replace runs: one
// that leaves a block the document cannot read back, or reads back as blocks of another kind, is
// refused naming replace_with, and nothing is written - the document stays byte for byte as it was
// and the suggestion stays open. An accept whose blocks read back as written is stored as before,
// including one that writes blocks, such as a rule or a list over a whole paragraph.
func TestAcceptingASuggestionRefusesAReplacementTheDocumentCannotCarryBack(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	const (
		paragraph  = "Intro.\n\nBody.\n\nAfter.\n"
		listItem   = "Intro.\n\n- Body.\n- two\n"
		blockquote = "Intro.\n\n> Body.\n"
		callout    = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n"
		footnote   = "x[^1]\n\n[^1]: Body.\n"
	)
	text := func(artifactID string) string {
		markdown, err := documentService.Text(context.Background(), artifactID)
		if err != nil {
			t.Fatal(err)
		}
		return markdown
	}
	suggest := func(t *testing.T, key, spec, with string) (string, model.Comment) {
		t.Helper()
		issue := createInteractionIssue(t, handler, key, "accept "+with, spec)
		created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": "Body."},
			"suggestion": map[string]string{"replace_with": with}, "actor": sessionActor(),
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
		}
		return issue.PrimaryArtifactID, decodeBody[model.Comment](t, created)
	}
	for index, test := range []struct{ name, spec, with string }{
		{"colons that read as a fence, in a paragraph", paragraph, ":::\nb"},
		{"a rule in a list item", listItem, "***"},
		{"a list in a list item", listItem, "- a"},
		{"two paragraphs in a list item", listItem, "a\n\nb"},
		{"code in a list item", listItem, "```\nc\n```"},
		{"colons that read as a fence, in a blockquote", blockquote, ":::\nb"},
		{"a fence in a callout", callout, ":::"},
		{"a rule in a footnote definition", footnote, "***"},
		{"a list in a footnote definition", footnote, "- a"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifactID, comment := suggest(t, "R"+string(rune('A'+index)), test.spec, test.with)
			before := text(artifactID)
			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
			if body := accepted.Body.String(); accepted.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) || !strings.Contains(body, "replace_with") {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, body)
			}
			if after := text(artifactID); after != before {
				t.Fatalf("after a refused accept = %q, want it unchanged, %q", after, before)
			}
			read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
			if read.Code != http.StatusOK || decodeBody[model.Comment](t, read).Resolved {
				t.Fatalf("after a refused accept the suggestion reads status=%d body=%s, want it open", read.Code, read.Body.String())
			}
		})
	}
	for index, test := range []struct{ name, spec, with, want string }{
		{"text", paragraph, "Changed.", "Intro.\n\nChanged.\n\nAfter.\n"},
		{"a rule over a paragraph", paragraph, "***", "Intro.\n\n---\n\nAfter.\n"},
		{"a list over a paragraph", paragraph, "- a", "Intro.\n\n- a\n\nAfter.\n"},
		{"two paragraphs over one", paragraph, "a\n\nb", "Intro.\n\na\n\nb\n\nAfter.\n"},
		{"dashes inside a line", paragraph, "--- a note", "Intro.\n\n--- a note\n\nAfter.\n"},
		{"text in a list item", listItem, "Changed.", "Intro.\n\n- Changed.\n- two\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifactID, comment := suggest(t, "K"+string(rune('A'+index)), test.spec, test.with)
			if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			if after := text(artifactID); after != test.want {
				t.Fatalf("after accepting = %q, want %q", after, test.want)
			}
		})
	}
}

// A code block's text is literal, so a replace there writes with exactly as sent - whitespace at
// its edges, markdown syntax, a reference, a tab - through the edits route and through an
// accepted suggestion alike.
func TestDocumentEditsReplaceInACodeBlockWritesWithAsSent(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	for index, test := range []struct {
		name, spec, find, with, want string
	}{
		{"a yaml item's indentation", "Intro.\n\n```yaml\n  - name: a\n  - name: b\n```\n", "  - name: a", "  - name: c", "Intro.\n\n```yaml\n  - name: c\n  - name: b\n```\n"},
		{"indentation added to the whole block", "Intro.\n\n```\nfoo\n```\n", "foo", "  foo", "Intro.\n\n```\n  foo\n```\n"},
		{"a tab", "Intro.\n\n```\nfoo\n```\n", "foo", "\tfoo", "Intro.\n\n```\n\tfoo\n```\n"},
		{"four spaces", "Intro.\n\n```\nfoo\n```\n", "foo", "    foo", "Intro.\n\n```\n    foo\n```\n"},
		{"emphasis syntax", "Intro.\n\n```\nfoo\n```\n", "foo", "**x** and `y`", "Intro.\n\n```\n**x** and `y`\n```\n"},
		{"a reference and an escape", "Intro.\n\n```\nfoo\n```\n", "foo", "a &amp; b\\*c", "Intro.\n\n```\na &amp; b\\*c\n```\n"},
		{"HTML", "Intro.\n\n```\nfoo\n```\n", "foo", "<div>x</div>", "Intro.\n\n```\n<div>x</div>\n```\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "C"+string(rune('A'+index)), test.name, test.spec)
			edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": test.find, "with": test.with}},
			}, "alice")
			if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"changed":true`) {
				t.Fatalf("replace: status=%d body=%s", edited.Code, edited.Body.String())
			}
			if markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || markdown != test.want {
				t.Fatalf("after replace = %q (%v), want %q", markdown, err, test.want)
			}
		})
	}
	issue := createInteractionIssue(t, handler, "CS", "Suggestion in a code block", "Intro.\n\n```\nfoo\n```\n")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "indent it", "anchor": map[string]any{"artifact": "spec", "quote": "foo"},
		"suggestion": map[string]string{"replace_with": "  foo &amp; **x**"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || markdown != "Intro.\n\n```\n  foo &amp; **x**\n```\n" {
		t.Fatalf("after accepting = %q (%v), want the replacement as sent", markdown, err)
	}
}

// A table is one addressable block. Deleting its header promotes the first body
// row so the table keeps its identity and remains valid Markdown.
func TestDocumentEditsDeleteTableHeaderRowInPlace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table row edit", "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_row", "block": tableID, "index": 0}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("delete table header row: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| A10 | old |\n| :--- | :--- |\n| A11 | new |\n"
	if text.Markdown != want {
		t.Fatalf("after header-row deletion = %q, want %q", text.Markdown, want)
	}
	blocks = decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	for _, block := range blocks {
		if block.Type == "table" {
			if block.ID != tableID {
				t.Fatalf("table block id after row deletion = %q, want %q", block.ID, tableID)
			}
			return
		}
	}
	t.Fatalf("blocks after row deletion = %#v, want table %q", blocks, tableID)
}

func TestDocumentEditsRejectInvalidTableIndicesWithoutChangingDocument(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "| Key | Value |\n| --- | --- |\n| A10 | old |\n| A11 | new |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Table index edit", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}
	const want = "| Key | Value |\n| :--- | :--- |\n| A10 | old |\n| A11 | new |\n"
	for _, test := range []struct {
		name     string
		hasIndex bool
		index    any
		detail   string
	}{
		{name: "missing", detail: "index is required"},
		{name: "negative", hasIndex: true, index: -1, detail: "index -1"},
		{name: "negative zero", hasIndex: true, index: json.RawMessage("-0"), detail: "index -0"},
		{name: "fractional", hasIndex: true, index: 1.5, detail: "index 1.5"},
		{name: "numeric string", hasIndex: true, index: "1", detail: `index "1"`},
		{name: "non-numeric string", hasIndex: true, index: "one", detail: `index "one"`},
		{name: "overflow", hasIndex: true, index: json.RawMessage("999999999999999999999999999999"), detail: "index 999999999999999999999999999999"},
		{name: "out of range", hasIndex: true, index: 3, detail: "row index 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			op := map[string]any{"op": "delete_row", "block": tableID}
			if test.hasIndex {
				op["index"] = test.index
			}
			rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{op},
			}, "alice")
			body := rejected.Body.String()
			message := decodeBody[struct {
				Error string `json:"error"`
			}](t, rejected)
			if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
				!strings.Contains(message.Error, `field "index"`) || !strings.Contains(message.Error, test.detail) ||
				!strings.Contains(message.Error, "3 rows") || !strings.Contains(message.Error, "2 columns") {
				t.Fatalf("invalid %s table index: status=%d body=%s", test.name, rejected.Code, body)
			}
			text := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
			if text.Markdown != want {
				t.Fatalf("invalid %s table index changed document = %q, want %q", test.name, text.Markdown, want)
			}
		})
	}
}

func TestDocumentEditsDeleteTableColumnInPlace(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Table column edit", "| Key | Value | Notes |\n| --- | --- | --- |\n| A10 | old | first |\n| A11 | new | second |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": tableID, "index": 1}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("delete table column: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| Key | Notes |\n| :--- | :--- |\n| A10 | first |\n| A11 | second |\n"
	if text.Markdown != want {
		t.Fatalf("after column deletion = %q, want %q", text.Markdown, want)
	}
	blocks = decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	for _, block := range blocks {
		if block.Type == "table" {
			if block.ID != tableID {
				t.Fatalf("table block id after column deletion = %q, want %q", block.ID, tableID)
			}
			return
		}
	}
	t.Fatalf("blocks after column deletion = %#v, want table %q", blocks, tableID)
}

func TestDocumentEditsCanonicalizeRaggedTableBeforeColumnDeletion(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ragged table column edit", "| One | Two | Three |\n| --- | --- | --- |\n| first | second | third |\n| only |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": tableID, "index": 1}},
	}, "alice")
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"applied":1`) {
		t.Fatalf("delete ragged table column: status=%d body=%s", edited.Code, edited.Body.String())
	}
	after := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| One | Three |\n| :--- | :--- |\n| first | third |\n| only |  |\n"
	if after.Markdown != want {
		t.Fatalf("ragged column deletion = %q, want %q", after.Markdown, want)
	}
}

func TestDocumentEditsRejectDeletionOfLastTableRowOrColumn(t *testing.T) {
	for _, test := range []struct {
		name  string
		op    string
		index int
	}{
		{name: "row", op: "delete_row", index: 1},
		{name: "column", op: "delete_column", index: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Last table "+test.name+" edit", "| Key |\n| --- |\n| A10 |\n")
			blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
			var tableID string
			for _, block := range blocks {
				if block.Type == "table" {
					tableID = block.ID
					break
				}
			}
			if tableID == "" {
				t.Fatalf("blocks = %#v, want a table", blocks)
			}
			before := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))

			rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": test.op, "block": tableID, "index": test.index}},
			}, "alice")
			body := rejected.Body.String()
			if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
				!strings.Contains(body, `field \"index\"`) || !strings.Contains(body, "last remaining") ||
				!strings.Contains(body, "2 rows") || !strings.Contains(body, "1 column") {
				t.Fatalf("last table %s: status=%d body=%s", test.name, rejected.Code, body)
			}
			after := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
			if after.Markdown != before.Markdown {
				t.Fatalf("last %s deletion changed document = %q, want %q", test.name, after.Markdown, before.Markdown)
			}
		})
	}
}

func TestDocumentEditsExplainCascadedBlockInAtomicBatch(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "- Parent\n  - Child\n"
	issue := createInteractionIssue(t, handler, "TEST", "Cascaded block edit", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var parentID, childID string
	for _, block := range blocks {
		if block.Type != "bullet_list" {
			continue
		}
		if block.From == 0 {
			parentID = block.ID
		} else {
			childID = block.ID
		}
	}
	if parentID == "" || childID == "" {
		t.Fatalf("blocks = %#v, want nested bullet lists", blocks)
	}
	before := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))

	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "delete", "block": parentID},
			{"op": "delete", "block": childID},
		},
	}, "alice")
	body := rejected.Body.String()
	if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
		!strings.Contains(body, `field \"block\"`) || !strings.Contains(body, "operation 0") ||
		!strings.Contains(body, "cascade") || !strings.Contains(body, parentID) {
		t.Fatalf("cascaded block batch: status=%d body=%s", rejected.Code, body)
	}
	after := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	if after.Markdown != before.Markdown {
		t.Fatalf("cascaded block batch changed document = %q, want %q", after.Markdown, before.Markdown)
	}
}

func TestDocumentEditsProtectLiveCellAnchorsAndListThemOnTheirTable(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "| Key | Status | Keep |\n| --- | --- | --- |\n| A10 | blocked | first |\n| A11 | later | second |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Table anchors", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}
	askResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Can this ship?",
		"anchor":   map[string]any{"artifact": "spec", "quote": "blocked"},
	}, "alice")
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create table ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[struct {
		ID string `json:"id"`
	}](t, askResponse)
	commentResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "Investigate this status.",
		"anchor": map[string]any{"artifact": "spec", "quote": "blocked"},
	}, "alice")
	if commentResponse.Code != http.StatusCreated {
		t.Fatalf("create table comment: status=%d body=%s", commentResponse.Code, commentResponse.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, commentResponse)

	blocks = decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	foundTable := false
	for _, block := range blocks {
		if block.Type == "table" {
			foundTable = true
			if block.References.Asks != 1 || block.References.Comments != 1 {
				t.Fatalf("table references = %#v, want one ask and one comment", block.References)
			}
			break
		}
	}
	if !foundTable {
		t.Fatalf("blocks after anchors = %#v, want a table", blocks)
	}
	before := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	for _, test := range []struct {
		name  string
		op    string
		index int
	}{
		{name: "row", op: "delete_row", index: 1},
		{name: "column", op: "delete_column", index: 1},
	} {
		t.Run("open anchors block "+test.name+" deletion", func(t *testing.T) {
			rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": test.op, "block": tableID, "index": test.index}},
			}, "alice")
			body := rejected.Body.String()
			if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
				!strings.Contains(body, `field \"index\"`) || !strings.Contains(body, test.name+" index") ||
				!strings.Contains(body, ask.ID) || !strings.Contains(body, comment.ID) {
				t.Fatalf("delete table %s with live anchors: status=%d body=%s", test.name, rejected.Code, body)
			}
			after := decodeBody[struct {
				Markdown string `json:"markdown"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
			if after.Markdown != before.Markdown {
				t.Fatalf("live anchors did not block %s deletion: %q, want %q", test.name, after.Markdown, before.Markdown)
			}
		})
	}
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]string{"text": "Yes."}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer table ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", nil, "alice"); resolved.Code != http.StatusOK {
		t.Fatalf("resolve table comment: status=%d body=%s", resolved.Code, resolved.Body.String())
	}
	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "delete_column", "block": tableID, "index": 1}},
	}, "alice")
	if edited.Code != http.StatusOK {
		t.Fatalf("delete table column with historical anchors: status=%d body=%s", edited.Code, edited.Body.String())
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| Key | Keep |\n| :--- | :--- |\n| A10 | first |\n| A11 | second |\n"
	if text.Markdown != want {
		t.Fatalf("table after deleting historical anchors = %q, want %q", text.Markdown, want)
	}
}

func TestDocumentEditsExplainCascadedTableInAtomicBatch(t *testing.T) {
	handler := newTestHandler(t)
	const markdown = "| Key |\n| --- |\n| A10 |\n"
	issue := createInteractionIssue(t, handler, "TEST", "Cascaded table edit", markdown)
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	var tableID string
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
			break
		}
	}
	if tableID == "" {
		t.Fatalf("blocks = %#v, want a table", blocks)
	}
	rejected := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{
			{"op": "delete", "block": tableID},
			{"op": "delete_row", "block": tableID, "index": 0},
		},
	}, "alice")
	body := rejected.Body.String()
	if rejected.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) ||
		!strings.Contains(body, `field \"block\"`) || !strings.Contains(body, "operation 0") ||
		!strings.Contains(body, tableID) {
		t.Fatalf("cascaded table batch: status=%d body=%s", rejected.Code, body)
	}
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"))
	const want = "| Key |\n| :--- |\n| A10 |\n"
	if text.Markdown != want {
		t.Fatalf("cascaded table batch changed document = %q, want %q", text.Markdown, want)
	}
}
