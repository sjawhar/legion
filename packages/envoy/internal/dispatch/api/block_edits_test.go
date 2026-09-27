package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// opens a block keeps to a line. The refusal names no Go type.
func TestDocumentEditsRefuseBlockHTMLWithAdviceToKeepItInsideALine(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TA", "block HTML", "Intro.\n\nBody.\n")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": "<div>x</div>"}},
	}, "alice")
	body := response.Body.String()
	if response.Code != http.StatusBadRequest || !strings.Contains(body, `"code":"INVALID_OP"`) {
		t.Fatalf("replace: status=%d body=%s", response.Code, body)
	}
	for _, want := range []string{"HTML", "inside a line"} {
		if !strings.Contains(body, want) {
			t.Fatalf("refusal %s lacks %q", body, want)
		}
	}
	if strings.Contains(body, "*ast.") {
		t.Fatalf("refusal %s names a Go type", body)
	}
}

// Two lists of one kind side by side are written with different markers, so a write that leaves
// them so - by deleting or emptying what stood between them, inserting a list beside one, or
// accepting a list beside one - is stored as the two lists it made, not one.
func TestAdjacentListsAnEditLeavesReadBackAsTwoLists(t *testing.T) {
	handler := newTestHandler(t)
	for index, test := range []struct {
		name, spec, want string
		op               map[string]any
		accept           string
		// items is how many items the two lists hold together, two when unset.
		items int
	}{
		{name: "a delete of the paragraph between them", spec: "- a\n\nBetween.\n\n- b\n", want: "- a\n\n* b\n",
			op: map[string]any{"op": "delete", "find": "Between."}},
		{name: "a replace that empties the paragraph between them", spec: "- a\n\nBetween.\n\n- b\n", want: "- a\n\n\n\n* b\n",
			op: map[string]any{"op": "replace", "find": "Between.", "with": ""}},
		{name: "an insert of a list before one", spec: "Intro.\n\n- y\n", want: "Intro.\n\n- x\n\n* y\n",
			op: map[string]any{"op": "insert", "after": "Intro.", "markdown": "- x"}},
		{name: "an insert of an ordered list before one", spec: "Intro.\n\n1. y\n", want: "Intro.\n\n1. x\n\n1) y\n",
			op: map[string]any{"op": "insert", "after": "Intro.", "markdown": "1. x"}},
		{name: "an accepted list over the paragraph before one", spec: "Intro.\n\nBody.\n\n- y\n", want: "Intro.\n\n- x\n\n* y\n",
			accept: "- x\n"},
		{name: "a delete beside a list whose item opens with a rule", spec: "- a\n\nBetween.\n\n- ***\n", want: "- a\n\n* ---\n",
			op: map[string]any{"op": "delete", "find": "Between."}},
		{name: "a delete beside a list whose later item opens with a rule", spec: "- a\n\nBetween.\n\n- b\n- ***\n", want: "- a\n\n* b\n* ---\n",
			op: map[string]any{"op": "delete", "find": "Between."}, items: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "AL"+string(rune('A'+index)), test.name, test.spec)
			if test.accept != "" {
				created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
					"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": "Body."},
					"suggestion": map[string]string{"replace_with": test.accept}, "actor": sessionActor(),
				})
				if created.Code != http.StatusCreated {
					t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
				}
				comment := decodeBody[model.Comment](t, created)
				if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
					t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
				}
			} else {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
					"ops": []map[string]any{test.op},
				}, "alice")
				if response.Code != http.StatusOK {
					t.Fatalf("edit: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
			if markdown != test.want {
				t.Fatalf("stored %q, want %q", markdown, test.want)
			}
			back, err := pmdoc.Parse(markdown)
			if err != nil {
				t.Fatalf("stored %q does not read back: %v", markdown, err)
			}
			lists, items := 0, 0
			for _, block := range back.Children {
				if block.Type == "bullet_list" || block.Type == "ordered_list" {
					lists++
					items += len(block.Children)
				}
			}
			if lists != 2 {
				t.Fatalf("stored %q reads back holding %d lists, want 2", markdown, lists)
			}
			if want := max(test.items, 2); items != want {
				t.Fatalf("stored %q reads back holding %d items, want %d", markdown, items, want)
			}
		})
	}
}

// An empty with that empties a container's paragraph is taken: a list item, a blockquote or a
// typed block holding only an empty paragraph reads back holding it, as the browser editor's
// parser reads it, and so does a list item whose first block is an emptied paragraph. An ask's
// only option emptied is refused, since an option needs a label.
func TestDocumentEditsEmptyingAContainersParagraphIsTaken(t *testing.T) {
	handler := newTestHandler(t)
	for index, test := range []struct{ name, spec string }{
		{"a list item", "- Body.\n- two\n"},
		{"a blockquote", "Intro.\n\n> Body.\n"},
		{"a typed block", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n"},
		{"a list item holding more", "- Body.\n\n  ```\n  code\n  ```\n"},
		{"a list, a callout's only block", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n- Body.\n:::\n"},
		{"a blockquote, a callout's only block", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n> Body.\n:::\n"},
		{"a list item's first paragraph of two", "- Body.\n\n  more\n- two\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "T"+string(rune('A'+index)), test.name, test.spec)
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": ""}},
			}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("replace: status=%d body=%s", response.Code, response.Body.String())
			}
			markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID)
			back, err := pmdoc.Parse(markdown)
			if err != nil {
				t.Fatalf("stored %q does not read back: %v", markdown, err)
			}
			if strings.Contains(markdown, "Body.") {
				t.Fatalf("stored %q still holds the emptied text", markdown)
			}
			again, err := pmdoc.Render(back)
			if err != nil || again != markdown {
				t.Fatalf("stored %q reads back as %q (%v)", markdown, again, err)
			}
		})
	}
	issue := createInteractionIssue(t, handler, "TZ", "an ask's only option", "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- Body.\n:::\n")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": ""}},
	}, "alice")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_ASK_BLOCK"`) {
		t.Fatalf("emptying an ask's only option: status=%d body=%s", response.Code, response.Body.String())
	}
}

// A list item whose first paragraph is empty is written with its next block on the marker's line,
// so a rule there is written `***`: `- ---` would be a thematic break at the list's level, and the
// item and the list around it would be lost, here and in the browser editor. A task item's emptied
// first paragraph beside another block is refused, since the browser editor reads no form of that
// item as a task: the marker's line would carry the next block as the task's text.
func TestDocumentEditsEmptyingAListItemsFirstParagraphKeepsTheItem(t *testing.T) {
	handler := newTestHandler(t)
	for index, test := range []struct{ name, spec, with, want string }{
		{"a rule after it", "- Body.\n\n  ***\n- two\n", "", "- ***\n- two\n"},
		{"a rule an edit does not touch", "- ***\n\nBody.\n", "x", "- ***\n\nx\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "L"+string(rune('A'+index)), test.name, test.spec)
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": test.with}},
			}, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("replace: status=%d body=%s", response.Code, response.Body.String())
			}
			if markdown := documentMarkdown(t, handler, issue.PrimaryArtifactID); markdown != test.want {
				t.Fatalf("stored %q, want %q", markdown, test.want)
			}
		})
	}
	for index, spec := range []string{"- [ ] Body.\n  - sub\n", "- [ ] Body.\n\n  ```\n  code\n  ```\n"} {
		issue := createInteractionIssue(t, handler, "LT"+string(rune('A'+index)), "a task item", spec)
		before := documentMarkdown(t, handler, issue.PrimaryArtifactID)
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": ""}},
		}, "alice")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(response.Body.String(), "task") {
			t.Fatalf("emptying a task item's first paragraph in %q: status=%d body=%s", spec, response.Code, response.Body.String())
		}
		if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
			t.Fatalf("a refused replace changed the document: %q, was %q", after, before)
		}
	}
}

// The browser editor's parser ends a typed block at a line of at least its fence's colons starting
// less than four columns past the typed block's own lines, even inside fenced code the typed block
// holds; list markers add their width to a code line's indentation, a tab advances to the next
// multiple of four from the column it stands at - two columns in a callout inside a blockquote or
// a list item - and a line in a blockquote closes nothing. The renderer writes each typed block's fence longer than every
// such line, so a replace or a suggestion that writes one into code inside a callout is stored with
// the code as sent (a CR LF written as a line feed), under a longer fence, and the document reads
// back whole - here, and in the
// engine (pmdoc.TestTypedFencesReadTheSameInTheEngineAndHere holds the engine's reading of each
// shape).
func TestDocumentEditsWriteAColonLineIntoCodeInsideATypedBlock(t *testing.T) {
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
	readsBack := func(markdown, code, fence string) {
		t.Helper()
		back, err := pmdoc.Parse(markdown)
		if err != nil || len(back.Children) != 3 {
			t.Fatalf("%q does not read back as three blocks (%v)", markdown, err)
		}
		var got string
		pmdoc.Walk(back, func(node *pmdoc.Node) bool {
			if node.Type == "code_block" && got == "" {
				got = node.Children[0].Text
			}
			return true
		})
		if got != code {
			t.Fatalf("%q reads back holding the code %q, want %q", markdown, got, code)
		}
		if fence != "" && !strings.Contains(markdown, "\n"+fence+"callout{") && !strings.Contains(markdown, " "+fence+"callout{") {
			t.Fatalf("%q does not open the callout with %s", markdown, fence)
		}
	}
	for index, test := range []struct{ name, spec, with, fence string }{
		{"a closing line", direct, "a\n:::\nb", "::::"},
		{"with a trailing space", direct, "a\n::: \nb", "::::"},
		{"opening the code", direct, ":::\nb", "::::"},
		{"ending the code", direct, "a\n:::", "::::"},
		{"indented two spaces", direct, "a\n  :::\nb", "::::"},
		{"indented three spaces", direct, "a\n   :::\nb", "::::"},
		{"four colons", direct, "a\n::::\nb", ":::::"},
		{"in a list item's code", listItem, "a\n:::\nb", "::::"},
		{"indented one space in a list item's code", listItem, "a\n :::\nb", "::::"},
		{"a tab in code in a callout in a blockquote", calloutInQuote, "a\n\t:::\nb", "::::"},
		{"a tab in code in a callout in a list item", calloutInItem, "a\n\t:::\nb", "::::"},
		{"a space and a tab in code in a callout in a list item", calloutInItem, "a\n \t:::\nb", "::::"},
		{"a line of four colons ending in CR LF", direct, "a\r\n::::\r\nb", ":::::"},
		{"a line ending in CR LF in a callout in a list item", calloutInItem, "a\r\n :::\r\nb", "::::"},
		{"indented four spaces", direct, "a\n    :::\nb", ":::"},
		{"indented a tab", direct, "a\n\t:::\nb", ":::"},
		{"a no-break space after the colons in a list item's code", listItem, "a\n:::\u00a0\nb", ":::"},
		{"indented two spaces in a list item's code", listItem, "a\n  :::\nb", ":::"},
		{"in a blockquote's code", quoted, "a\n:::\nb", ":::"},
		{"in code outside a typed block", outside, "a\n:::\nb", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "E"+string(rune('A'+index)), "colons in code", test.spec)
			edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": "Body.", "with": test.with}},
			}, "alice")
			if edited.Code != http.StatusOK {
				t.Fatalf("replace: status=%d body=%s", edited.Code, edited.Body.String())
			}
			readsBack(text(issue.PrimaryArtifactID), pmdoc.LineFeeds(test.with), test.fence)
			suggested := createInteractionIssue(t, handler, "S"+string(rune('A'+index)), "colons in code", test.spec)
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+suggested.Key+"/comments", map[string]any{
				"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": "Body."},
				"suggestion": map[string]string{"replace_with": test.with}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)
			if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			readsBack(text(suggested.PrimaryArtifactID), pmdoc.LineFeeds(test.with), test.fence)
		})
	}
}

// An accept whose blocks read back as written is stored, including one that writes blocks, such
// as a rule or a list over a whole paragraph, and a line of dashes is such a rule. Blocks at a
// list item's start follow its empty first paragraph, and an emptied paragraph is not written
// where the rest of its block stands without it. Where the text lands at the document's start, a
// closed front-matter block that opens it is front matter. What the live document holds after a
// non-empty accept is what its markdown reads back as: the token of a document seeded with the
// stored markdown is the accepted document's, so two paragraphs in a tight list item leave the
// item spread, and a block over an item's text leaves no empty paragraph before its nested list.
func TestAcceptingASuggestionStoresBlocksTheDocumentReadsBack(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	const (
		paragraph  = "Intro.\n\nBody.\n\nAfter.\n"
		listItem   = "Intro.\n\n- Body.\n- two\n"
		longItem   = "Intro.\n\n- Body.\n\n  more\n- two\n"
		ordered    = "Intro.\n\n1. Body.\n2. two\n"
		nestedItem = "Intro.\n\n- Body.\n  - nested\n- two\n"
		blockquote = "Intro.\n\n> Body.\n"
		callout    = "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n"
		footnote   = "x[^1]\n\n[^1]: Body.\n"
		code       = "Intro.\n\n```\nBody.\n```\n"
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
	for index, test := range []struct{ name, spec, with, want string }{
		{"text", paragraph, "Changed.", "Intro.\n\nChanged.\n\nAfter.\n"},
		{"a rule over a paragraph", paragraph, "***", "Intro.\n\n---\n\nAfter.\n"},
		// An accept's text goes inside the document, so a leading `---` is a rule, as `***` is,
		// never the front matter that would open a document and swallow the line.
		{"dashes over a paragraph", paragraph, "---", "Intro.\n\n---\n\nAfter.\n"},
		{"dashes inside a paragraph", "Intro.\n\nSay Body. now.\n\nAfter.\n", "---", "Intro.\n\nSay&#32;\n\n---\n\n&#32;now.\n\nAfter.\n"},
		{"a list over a paragraph", paragraph, "- a", "Intro.\n\n- a\n\nAfter.\n"},
		{"two paragraphs over one", paragraph, "a\n\nb", "Intro.\n\na\n\nb\n\nAfter.\n"},
		{"dashes inside a line", paragraph, "--- a note", "Intro.\n\n--- a note\n\nAfter.\n"},
		{"text in a list item", listItem, "Changed.", "Intro.\n\n- Changed.\n- two\n"},
		{"two paragraphs in a list item", listItem, "a\n\nb", "Intro.\n\n- a\n\n  b\n- two\n"},
		{"a rule in a list item", listItem, "***", "Intro.\n\n- ***\n- two\n"},
		{"a list in a list item", listItem, "- a", "Intro.\n\n- - a\n- two\n"},
		{"code in a list item", listItem, "```\nc\n```", "Intro.\n\n- ```\n  c\n  ```\n- two\n"},
		{"a list in a list item holding two paragraphs", longItem, "- a", "Intro.\n\n- - a\n\n  more\n- two\n"},
		{"a list in an ordered item", ordered, "- a", "Intro.\n\n1. - a\n2. two\n"},
		{"nothing in a list item", listItem, "", "Intro.\n\n- \n- two\n"},
		{"nothing in a list item holding two paragraphs", longItem, "", "Intro.\n\n- more\n- two\n"},
		{"nothing in a list item holding a nested list", nestedItem, "", "Intro.\n\n- - nested\n- two\n"},
		{"a heading in a list item holding a nested list", nestedItem, "# H", "Intro.\n\n- # H\n  - nested\n- two\n"},
		{"code in a list item holding a nested list", nestedItem, "```\nc\n```", "Intro.\n\n- ```\n  c\n  ```\n  - nested\n- two\n"},
		// Code text is written literally, but markdown drops the line breaks that end it, so an
		// accept stores its code without them, as it reads back.
		{"code text ending in a backslash and a line break", code, "x\\\n", "Intro.\n\n```\nx\\\n```\n"},
		{"code text ending in spaces and a line break", code, "x  \n", "Intro.\n\n```\nx  \n```\n"},
		// A line holding only whitespace in a list item's code reads back empty, so an accept
		// writes the line it brings so.
		{"a whitespace line in a list item's code", "- ```\n  Body.\n  ```\n", " ", "- ```\n  \n  ```\n"},
		{"a form-feed line in a list item's code", "- ```\n  a\n  Body.\n  b\n  ```\n", "\f", "- ```\n  a\n  \f\n  b\n  ```\n"},
		{"a no-break space line in a list item's code", "- ```\n  a\n  Body.\n  b\n  ```\n", "\u00a0", "- ```\n  a\n  \u00a0\n  b\n  ```\n"},
		{"a vertical tab line in a list item's code", "- ```\n  a\n  Body.\n  b\n  ```\n", "\v", "- ```\n  a\n  \v\n  b\n  ```\n"},
		{"an ideographic space line in a list item's code", "- ```\n  a\n  Body.\n  b\n  ```\n", "\u3000", "- ```\n  a\n  \u3000\n  b\n  ```\n"},
		{"nothing between no-break spaces on a line of a list item's code", "- ```\n  \u00a0Body.\u00a0\n  ```\n", "", "- ```\n  \u00a0\u00a0\n  ```\n"},
		{"text ending in a whitespace line at the end of a list item's code", "- ```\n  Body.\n  ```\n", "x\n  ", "- ```\n  x\n  ```\n"},
		{"nothing over an indented line of a list item's code", "- item\n\n  ```\n    Body.\n  ```\n", "", "- item\n\n  ```\n  \n  ```\n"},
		{"two paragraphs beside an item holding a nested list", "- Body.\n- two\n  - nested\n", "x\n\ny", "- x\n\n  y\n- two\n\n  - nested\n"},
		{"two paragraphs beside an item holding a quote, in an ordered list", "1. Body.\n2. two\n   > q\n", "x\n\ny", "1. x\n\n   y\n2. two\n\n   > q\n"},
		// An emptied footnote definition holds one empty paragraph, which reads back as the
		// definition, so its reference stays a reference.
		{"nothing in a footnote definition", footnote, "", "x[^1]\n\n[^1]: \n"},
		{"a rule in a footnote definition", footnote, "***", "x[^1]\n\n[^1]: ---\n"},
		{"a list in a footnote definition", footnote, "- a", "x[^1]\n\n[^1]: - a\n"},
		{"front matter over an opening heading", "# Body.\n\nText.\n", "---\nstatus: draft\n---\n\n# Body.", "---\nstatus: draft\n---\n\n# Body.\n\nText.\n"},
		{"front matter over an opening paragraph", "Body.\n\nAfter.\n", "---\ntitle: x\n---\n\nBody.", "---\ntitle: x\n---\n\nBody.\n\nAfter.\n"},
		// A line of colons in text is written escaped, so it reads back as the text it is and never
		// as a typed block's fence.
		{"colons in a paragraph", paragraph, ":::\nb", "Intro.\n\n\\::: b\n\nAfter.\n"},
		{"colons in a blockquote", blockquote, ":::\nb", "Intro.\n\n> \\::: b\n"},
		{"colons in a callout", callout, ":::", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n\\:::\n:::\n"},
		// A document that writes nothing reads back as the one empty paragraph it holds.
		{"nothing over a document's only paragraph", "Body.\n", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifactID, comment := suggest(t, "K"+string(rune('A'+index/26))+string(rune('A'+index%26)), test.spec, test.with)
			if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			if after := text(artifactID); after != test.want {
				t.Fatalf("after accepting = %q, want %q", after, test.want)
			}
			if test.with == "" {
				// An emptied paragraph stays in the live document where the markdown drops it.
				return
			}
			_, token, err := documentService.TextWithToken(context.Background(), artifactID)
			if err != nil {
				t.Fatal(err)
			}
			seeded := createInteractionIssue(t, handler, "S"+string(rune('A'+index/26))+string(rune('A'+index%26)), "seeded "+test.name, test.want)
			if _, readBack, err := documentService.TextWithToken(context.Background(), seeded.PrimaryArtifactID); err != nil || readBack != token {
				t.Fatalf("the accepted document's token = %s, but its markdown reads back as %s (%v)", token, readBack, err)
			}
		})
	}
	// An unclosed `---` that opens the document is a rule there, as `***` is.
	opening := map[string]string{}
	for index, with := range []string{"---", "***"} {
		artifactID, comment := suggest(t, "H"+string(rune('A'+index)), "# Body.\n\nText.\n", with)
		if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
			t.Fatalf("accept %q: status=%d body=%s", with, accepted.Code, accepted.Body.String())
		}
		opening[with] = text(artifactID)
	}
	if opening["---"] != opening["***"] {
		t.Fatalf("accepting `---` over an opening heading = %q, want what `***` writes, %q", opening["---"], opening["***"])
	}
}

// acceptCase is a suggestion of with over quote in a document seeded with spec, after replacing
// emptied's text with refill through the edit route (as an edit can leave a document that already
// reads back otherwise), and what accepting it does: refused naming says, or stored as want.
type acceptCase struct{ name, spec, quote, with, emptied, refill, says, want string }

// checkAccept accepts test's suggestion and checks the outcome. A refusal is 400 INVALID_OP naming
// test.says, with the document unchanged and the suggestion open. A stored accept reads back as
// the live document: a document seeded with the stored markdown holds the accepted one's token,
// except after an empty replacement, whose emptied paragraph stays in the live document, and in a
// document the edit left reading back otherwise already.
func checkAccept(t *testing.T, handler http.Handler, documentService *docs.Service, key string, test acceptCase) {
	t.Helper()
	issue := createInteractionIssue(t, handler, key, test.name, test.spec)
	if test.emptied != "" {
		edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]any{{"op": "replace", "find": test.emptied, "with": test.refill}},
		}, "alice")
		if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"changed":true`) {
			t.Fatalf("empty %q: status=%d body=%s", test.emptied, edited.Code, edited.Body.String())
		}
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": test.quote},
		"suggestion": map[string]string{"replace_with": test.with}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	before, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	stored, token, err := documentService.TextWithToken(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if test.says != "" {
		if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), `"code":"INVALID_OP"`) || !strings.Contains(accepted.Body.String(), test.says) {
			t.Fatalf("accept: status=%d body=%s, want 400 INVALID_OP saying %q", accepted.Code, accepted.Body.String(), test.says)
		}
		if stored != before {
			t.Fatalf("after refused accept = %q, want unchanged %q", stored, before)
		}
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
		if read.Code != http.StatusOK || decodeBody[model.Comment](t, read).Resolved {
			t.Fatalf("suggestion after refusal: status=%d body=%s, want it open", read.Code, read.Body.String())
		}
		return
	}
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if stored != test.want {
		t.Fatalf("after accepting = %q, want %q", stored, test.want)
	}
	if test.with == "" || test.emptied != "" {
		return
	}
	seeded := createInteractionIssue(t, handler, "S"+key, "seeded "+test.name, stored)
	if _, readBack, err := documentService.TextWithToken(context.Background(), seeded.PrimaryArtifactID); err != nil || readBack != token {
		t.Fatalf("the accepted document's token = %s, but its markdown reads back as %s (%v)", token, readBack, err)
	}
}

// An accept is judged by the whole document it would store. Two lists of one kind side by side
// are written with different markers, so they read back as two lists wherever an accept leaves
// them: a list written beside one, text that joins away what stood between two, or blocks an
// accept writes where a callout it rewrites or consumes stood. A task item emptied of its text
// reads back as a plain item, so emptying one is refused, naming that and advising rejecting. What
// the document already reads back otherwise is not the accept's: a task item read back as plain,
// a paragraph ending in a hard break that reads back with a literal backslash. Text elsewhere, or
// beside it in the same block, is stored, and a second task item emptied is still refused.
func TestAcceptingASuggestionIsJudgedByTheDocumentItStores(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	const (
		callout = ":::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::"
		task    = "leaves blocks the document reads back otherwise (a task item reads back as a plain list item); reject the suggestion"
	)
	for index, test := range []acceptCase{
		{name: "a list before a list", spec: "Intro.\n\nBody.\n\n- y\n", quote: "Body.", with: "- x", want: "Intro.\n\n- x\n\n* y\n"},
		{name: "a list after a list", spec: "Intro.\n\n- y\n\nBody.\n", quote: "Body.", with: "- x", want: "Intro.\n\n- y\n\n* x\n"},
		{name: "an ordered list before an ordered list", spec: "Intro.\n\nBody.\n\n1. y\n", quote: "Body.", with: "1. x", want: "Intro.\n\n1. x\n\n1) y\n"},
		{name: "two bullet lists", spec: "Intro.\n\nBody.\n\nAfter.\n", quote: "Body.", with: "- a\n\n* b", want: "Intro.\n\n- a\n\n* b\n\nAfter.\n"},
		{name: "nothing between two lists", spec: "- a\n\nBody.\n\n- b\n", quote: "Body.", want: "- a\n\n\n\n* b\n"},
		{name: "text joining a list item to the paragraph before a list", spec: "- y\n- Body.\n\nAfter here.\n\n- z\n", quote: "Body. After", with: "x", want: "- y\n- x here.\n\n* z\n"},
		{name: "a list beside a callout's rewrite, before a list", spec: "- y\n\n" + callout + "\n", quote: "Body.", with: "- x\n\n" + callout, want: "- y\n\n* x\n\n" + callout + "\n"},
		{name: "a list consuming a callout beside a list", spec: "Intro.\n\n" + callout + "\n\n- After here.\n", quote: "Body. After", with: "- a", want: "Intro.\n\n- a\n\n* &#32;here.\n"},
		{name: "nothing in a task item", spec: "- [ ] Body.\n- [x] two\n", quote: "Body.", says: task},
		{name: "nothing in a task item, in a document already reading another back as a plain item", spec: "- [ ] Done\n\nIntro.\n\n- [ ] Body.\n", quote: "Body.", emptied: "Done", says: task},
		{name: "text in a document already reading a task item back as a plain item", spec: "- [ ] Done\n\nBody.\n", quote: "Body.", emptied: "Done", with: "Changed.", want: "- [ ] \n\nChanged.\n"},
		// A block that already reads back otherwise the same way is not the accept's, though the
		// accept changes it: text beside a stale break in it is stored, a new break is refused.
		{name: "text beside a task item already read back as plain, in its list", spec: "- [ ] Gone.\n- [x] Body.\n\nAfter.\n", quote: "Body.", emptied: "Gone.", with: "Changed.", want: "- [ ] \n- [x] Changed.\n\nAfter.\n"},
		{name: "nothing in a task item, in a list already reading another back as plain", spec: "- [ ] Gone.\n- [x] Body.\n", quote: "Body.", emptied: "Gone.", says: task},
		{name: "two paragraphs in a task item, in a list already reading another back as plain", spec: "- [ ] Gone.\n- [x] Body.\n\nAfter.\n", quote: "Body.", emptied: "Gone.", with: "two\n\nparas", want: "- [ ] \n- [x] two\n\n  paras\n\nAfter.\n"},
		{name: "text from code into the paragraph after it", spec: "Intro.\n\n```\nabc\n```\n\nNext here.\n", quote: "abc Next", with: "x", want: "Intro.\n\n```\nx here.\n```\n"},
		{name: "text from a list item's code into the next item", spec: "- ```\n  abc\n  ```\n- Next\n", quote: "abc Next", with: "x", want: "- ```\n  x\n  ```\n"},
		{name: "text ending in a break from code through the whole next paragraph", spec: "Intro.\n\n```\nabc\n```\n\nNext here.\n", quote: "abc Next here.", with: "x\n", want: "Intro.\n\n```\nx\n```\n"},
		{name: "text ending in a break from a list item's code through the whole next item", spec: "- ```\n  abc\n  ```\n- Next\n", quote: "abc Next", with: "x\n", want: "- ```\n  x\n  ```\n"},
		{name: "text ending in breaks from a list item's code through a whole nested item", spec: "- ```\n  abc\n  ```\n  - Next\n", quote: "abc Next", with: "x\n\n", want: "- ```\n  x\n  ```\n"},
		{name: "text ending in a break from a list item's code through the item's whole paragraph", spec: "- ```\n  abc\n  ```\n\n  Next\n", quote: "abc Next", with: "x\n", want: "- ```\n  x\n  ```\n"},
		{name: "text ending in a blank line from a list item's code through the whole next item", spec: "- ```\n  abc\n  ```\n- Next\n", quote: "abc Next", with: "x\n  ", want: "- ```\n  x\n  ```\n"},
		{name: "text from code into a table cell", spec: "Intro.\n\n```\nabc\n```\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "X", want: "Intro.\n\n```\nX\n```\n\n|  | b |\n| :--- | :--- |\n| c | d |\n"},
		{name: "text from a paragraph into a table cell", spec: "Intro abc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "x", want: "Intro x\n\n|  | b |\n| :--- | :--- |\n| c | d |\n"},
		{name: "text from code in a callout into a table after the callout", spec: ":::callout{#c1 kind=\"note\" title=\"T\"}\n```\nabc\n```\n:::\n\n| Next | b |\n| :--- | :--- |\n| x | y |\n", quote: "abc Next", with: "x", want: ":::callout{#c1 kind=\"note\" title=\"T\"}\n```\nx\n```\n:::\n\n|  | b |\n| :--- | :--- |\n| x | y |\n"},
		{name: "text from code emptying the next task item ahead of its nested list", spec: "```\nabc\n```\n\n- [ ] Next\n  - child\n", quote: "abc Next", with: "x", says: "changes how the bullet list reads back"},
		{name: "text from one table through the next table's header row", spec: "| AAA | BBB |\n| :--- | :--- |\n| CCC | DDD |\n\n| EEE | FFF |\n| :--- | :--- |\n| GGG | HHH |\n", quote: "DDD EEE FFF", with: "x", says: "leaves text the document reads back as another block where it lands (a table reads back as a paragraph)"},
		{name: "text from one table's cell into the next table's body", spec: "| AAA | BBB |\n| :--- | :--- |\n| CCC | DDD |\n\n| EEE | FFF |\n| :--- | :--- |\n| GGG | HHH |\n", quote: "DD EEE FFF GG", with: "x", says: "leaves text the document reads back as another block where it lands (a table reads back as a paragraph)"},
		{name: "text from one one-column table into the next", spec: "| AA |\n| :--- |\n| BB |\n\n| CC |\n| :--- |\n| DD |\n", quote: "BB CC", with: "x", says: "leaves text the document reads back as another block where it lands (a table reads back as a paragraph)"},
		{name: "a list from a whole list item into a table cell", spec: "- abc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "- a", says: "writes a bullet list in this list item, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a list over three whole textblocks into a table cell", spec: "abc\n\nMid\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Mid Next", with: "- a", says: "writes a bullet list in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a list from a whole heading into a table cell", spec: "# abc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "- a", want: "- a\n\n|  | b |\n| :--- | :--- |\n| c | d |\n"},
		{name: "a list from one character into a paragraph into a table cell", spec: "Xabc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "- a", says: "writes a bullet list in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "two paragraphs from one character into a paragraph into a table cell", spec: "Xabc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "a\n\nb", says: "writes two paragraphs in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a heading from one character into a paragraph into a table cell", spec: "Xabc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "# H", says: "writes a heading in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a list from one character into a heading into a table cell", spec: "# Xabc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "- a", says: "writes a bullet list in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a list from part of a paragraph into a table cell", spec: "Intro abc\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "- a", says: "writes a bullet list in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a list from a callout's paragraph into a table cell", spec: ":::callout{#c1 kind=\"note\" title=\"T\"}\nabc\n:::\n\n| Next | b |\n| :--- | :--- |\n| c | d |\n", quote: "abc Next", with: "- a", says: "writes a bullet list in this callout, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "a list from two characters into a heading into a table's first cell", spec: "# PPP\n\n| AAA | BBB |\n| :--- | :--- |\n| CCC | DDD |\n", quote: "PP AAA", with: "- a", says: "writes a bullet list in this document, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "text from a body row's last cell into the next body row's first cell", spec: "| AAA | BBB |\n| :--- | :--- |\n| CCC | DDD |\n| EEE | FFF |\n", quote: "DD EE", with: "x", says: "leaves text the document reads back as another block where it lands (the table row's end reads back as a table cell)"},
		{name: "text from the header row's last cell into the first body row's first cell", spec: "| AAA | BBB |\n| :--- | :--- |\n| CCC | DDD |\n", quote: "BB CC", with: "x", want: "| AAA | BxC |\n| :--- | :--- |\n| DDD |  |\n"},
		{name: "a list from a body cell into the next row's cell", spec: "| H | I |\n| :--- | :--- |\n| c | aa |\n| bb e | f |\n", quote: "aa bb", with: "- a", says: "writes a bullet list in this table cell, which the document cannot read back there (the table row's end reads back as a table cell)"},
		{name: "a list over a table's first header cell", spec: "| Body. | b |\n| :---: | --- |\n| x | y |\n", quote: "Body.", with: "- a", says: "writes a bullet list in this table header, which the document cannot read back there (a table cell reads back as nothing)"},
		{name: "text ending in a break over code that already ends in one", spec: "Intro.\n\n```\nabc\n```\n", quote: "abc", emptied: "abc", refill: "abc\n", with: "xyz\n", want: "Intro.\n\n```\nxyz\n\n```\n"},
		{name: "text beside code the document already writes with ending breaks", spec: "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n\n```\nc\n```\n:::\n", quote: "Body.", emptied: "c", refill: "c\n\n", with: "Changed.", want: "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nChanged.\n\n```\nc\n\n\n```\n:::\n"},
		{name: "text in a paragraph already reading back with a literal backslash", spec: "Body. more\\\nxyz\n\nAfter.\n", quote: "Body.", emptied: "xyz", with: "Changed.", want: "Changed. more\\\n\n\nAfter.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkAccept(t, handler, documentService, "L"+string(rune('A'+index/26))+string(rune('A'+index%26)), test)
		})
	}
}

// An empty accept inside a typed block stores the block holding the one empty paragraph it reads
// back holding, and is refused where that does not read back, as in a list item, and for an ask,
// whose question paragraph+ bullet_list? cannot be empty.
func TestAcceptingAnEmptySuggestionInATypedBlock(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	for index, test := range []struct {
		name, spec, code, want string
	}{
		{"a top-level callout", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\nBody.\n:::\n", "", "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"T\"}\n\n:::\n"},
		{"a nested callout", "::::callout{#outer kind=\"note\" title=\"T\"}\n:::callout{#inner kind=\"note\" title=\"T\"}\nBody.\n:::\n::::\n", "", "::::callout{#outer kind=\"note\" title=\"T\"}\n:::callout{#inner kind=\"note\" title=\"T\"}\n\n:::\n::::\n"},
		{"a callout in a blockquote", "> :::callout{#c1 kind=\"note\" title=\"T\"}\n> Body.\n> :::\n", "", "> :::callout{#c1 kind=\"note\" title=\"T\"}\n> \n> :::\n"},
		{"a callout in a list item", "- Lead.\n\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n  Body.\n  :::\n", "INVALID_OP", ""},
		{"an ask", ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nBody.\n:::\n", "INVALID_ASK_BLOCK", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := createInteractionIssue(t, handler, "T"+string(rune('A'+index)), "empty typed block", test.spec)
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "suggest", "anchor": map[string]any{"artifact": "spec", "quote": "Body."},
				"suggestion": map[string]string{"replace_with": ""}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)
			before, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
			if err != nil {
				t.Fatal(err)
			}
			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
			if test.code == "" {
				if accepted.Code != http.StatusOK {
					t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
				}
				if after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || after != test.want {
					t.Fatalf("after accepting = %q (%v), want %q", after, err, test.want)
				}
				return
			}
			if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("accept: status=%d body=%s, want 400 %s", accepted.Code, accepted.Body.String(), test.code)
			}
			after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
			if err != nil || after != before {
				t.Fatalf("after refused accept = %q (%v), want unchanged %q", after, err, before)
			}
			read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")
			if read.Code != http.StatusOK || decodeBody[model.Comment](t, read).Resolved {
				t.Fatalf("suggestion after refusal: status=%d body=%s, want it open", read.Code, read.Body.String())
			}
		})
	}
}

// An accept in or around a typed block is judged by what the typed block reads back as: lists of
// one kind side by side in a callout are written with different markers, as they are in the
// blockquote a callout rewritten under its own id stands in, and read back as written. Blocks a
// same-id rewrite puts where the typed block stood are refused naming that block, a list item for
// an empty callout, which cannot be written there.
func TestAcceptingASuggestionAroundATypedBlock(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	const (
		callout = ":::callout{#c1 kind=\"note\" title=\"T\"}\n"
		rewrite = callout + "Body2.\n:::\n\n- x\n"
	)
	for index, test := range []acceptCase{
		{name: "a list beside a list in a callout", spec: "Intro.\n\n" + callout + "Body.\n\n- y\n:::\n", quote: "Body.", with: "- x", want: "Intro.\n\n" + callout + "- x\n\n* y\n:::\n"},
		{name: "a list beside a list in a blockquote inside a callout", spec: callout + "> Body.\n>\n> - y\n:::\n", quote: "Body.", with: "- x", want: callout + "> - x\n>\n> * y\n:::\n"},
		{name: "a callout rewritten beside a list in a blockquote", spec: "Intro.\n\n> " + callout + "> Body.\n> :::\n>\n> - y\n", quote: "Body.", with: rewrite,
			want: "Intro.\n\n> " + callout + "> Body2.\n> :::\n>\n> - x\n>\n> * y\n"},
		{name: "an empty callout beside a callout rewritten in a list item", spec: "- Lead.\n\n  " + callout + "  Body.\n  :::\n", quote: "Body.",
			with: callout + "Re.\n:::\n\n:::callout{kind=\"note\" title=\"T\"}\n:::", says: "writes two callouts in this list item"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkAccept(t, handler, documentService, "N"+string(rune('A'+index)), test)
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
