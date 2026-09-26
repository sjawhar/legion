package api

import (
	"context"
	"errors"

	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestQuoteAnchorWritesServerMark(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Quote anchor", "The quick brown fox")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Why brown?",
		"anchor":   map[string]any{"artifact": "spec", "quote": "brown"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create quote anchor: status=%d body=%s", created.Code, created.Body.String())
	}
	ask := decodeBody[struct {
		ID     string `json:"id"`
		Anchor struct {
			ArtifactID string `json:"artifact_id"`
			MarkID     string `json:"mark_id"`
			Version    int    `json:"version"`
			Quote      string `json:"quote"`
			Orphaned   bool   `json:"orphaned"`
		} `json:"anchor"`
	}](t, created)
	if ask.Anchor.ArtifactID != issue.PrimaryArtifactID || ask.Anchor.MarkID != ask.ID || ask.Anchor.Version != 1 || ask.Anchor.Quote != "brown" || ask.Anchor.Orphaned {
		t.Fatalf("quote anchor = %#v, want mark-backed brown at version 1", ask.Anchor)
	}
	if anchored, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkAsk, ask.ID); err != nil || anchored.Quote != "brown" {
		t.Fatalf("server-written ask mark = %q, %v", anchored.Quote, err)
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || strings.Contains(text.Body.String(), "<span") {
		t.Fatalf("marked document text: status=%d body=%s", text.Code, text.Body.String())
	}
}

func TestMarkAnchorVerifiesBrowserMark(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Browser mark", "The quick brown fox")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind: docs.MarkComment,
		ID:   "m-1",
		By:   model.Actor{Kind: "user", ID: "alice"},
	}, "quick")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "why",
		"anchor": map[string]any{"artifact": "spec", "mark_id": "m-1"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("record browser mark: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		ID     string `json:"id"`
		Anchor struct {
			MarkID string `json:"mark_id"`
			Quote  string `json:"quote"`
		} `json:"anchor"`
	}](t, created)
	if comment.Anchor.MarkID != "m-1" || comment.Anchor.Quote != "quick" {
		t.Fatalf("browser-mark anchor = %#v, want m-1 / quick", comment.Anchor)
	}
	if projection, found := awaitMarkProjection(t, database, issue.PrimaryArtifactID, "m-1"); !found || projection["text"] != "why" {
		t.Fatalf("browser-mark projection = %#v found=%t, want m-1 record", projection, found)
	}
	if _, found := findMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID); found {
		t.Fatalf("browser-mark projection unexpectedly uses row id %q", comment.ID)
	}
}

// writeBrowserMark stands in for a browser writing a mark into the live document. A real browser
// writes on its own Yjs client; this goes through the server's, so the comment's projection that
// follows is that client's next update, which the persisted document can only integrate once this
// mark is persisted too. Committing the mark in its own transaction makes it durable first, where
// ygo's persistence worker would otherwise store it on its own schedule.
func writeBrowserMark(t *testing.T, database *store.Store, documentService *docs.Service, artifactID string, mark docs.MarkSpec, quote string) {
	t.Helper()
	tx, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin browser mark: %v", err)
	}
	defer tx.Rollback(context.Background())
	ctx, ledger := documentService.Join(context.Background(), tx)
	defer ledger.Discard()
	if _, err := documentService.MarkQuote(ctx, artifactID, mark, quote, nil); err != nil {
		t.Fatalf("write browser mark: %v", err)
	}
	if err := ledger.Commit(context.Background()); err != nil {
		t.Fatalf("commit browser mark: %v", err)
	}
}

func TestMarkAnchorSuggestionProjectsBrowserKind(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Browser suggestion", "The quick brown fox")
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, "quick")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":       "new text",
		"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
		"suggestion": map[string]string{"replace_with": "new text"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("record browser insert suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	if projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, "m-insert"); projection["kind"] != "insert" {
		t.Fatalf("browser insert projection = %#v, want kind insert", projection)
	}
}

func TestQuoteAnchorCommentSpansParagraphs(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Cross-block comment", "one\n\ntwo")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "why", "anchor": map[string]any{"artifact": "spec", "quote": "one two"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create cross-block comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	if comment.Anchor == nil || comment.Anchor.Quote != "one two" {
		t.Fatalf("cross-block comment anchor = %#v, want one two", comment.Anchor)
	}
}

func TestSuggestionAcceptSpansParagraphs(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Cross-block suggestion", "one\n\ntwo")
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "replace both", "anchor": map[string]any{"artifact": "spec", "quote": "one two"},
		"suggestion": map[string]string{"replace_with": "replacement"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create cross-block suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusOK {
		t.Fatalf("accept cross-block suggestion: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if markdown, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || markdown != "replacement\n" {
		t.Fatalf("accepted cross-block document = %q (%v), want replacement", markdown, err)
	}
	if _, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID); !errors.Is(err, docs.ErrAnchorMissing) {
		t.Fatalf("cross-block suggestion mark = %v, want missing", err)
	}
}

// An ask block holds paragraphs and at most one bullet list. A suggestion on its question whose
// replacement also carries a code block would leave an ask that does not parse; the splice once
// fitted it by replacing the whole block, so the question someone may be waiting on vanished.
// Accepting refuses it with the edit route's own ask-block refusal, and the document and the
// suggestion stay as they were, also beside another ask that was already malformed: that one is
// not the accept's, but the ask it breaks is. The same text as a replace is refused by the edit
// route too.
func TestSuggestionAcceptRefusesAReplacementItsAskCannotHold(t *testing.T) {
	for _, test := range []struct{ name, spec string }{
		{name: "alone", spec: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\nAfter.\n"},
		{name: "beside a malformed ask", spec: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n" +
			strings.NewReplacer("#a1", "#a2", "Which one?", "Ship it?").Replace(malformedAsk)},
	} {
		t.Run(test.name, func(t *testing.T) { acceptBreakingAnAsk(t, test.spec) })
	}
}

func acceptBreakingAnAsk(t *testing.T, spec string) {
	t.Helper()
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Suggestion on an ask", spec)
	before, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil || !strings.Contains(before, ":::ask{#a1") {
		t.Fatalf("seeded document = %q (%v), want the ask block", before, err)
	}
	replacement := "Which?\n\n```\ncode\n```\n"
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "reword it", "anchor": map[string]any{"artifact": "spec", "quote": "Which one?"},
		"suggestion": map[string]string{"replace_with": replacement}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)

	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

	if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), `"code":"INVALID_ASK_BLOCK"`) ||
		!strings.Contains(accepted.Body.String(), `ask block \"a1\"`) {
		t.Fatalf("accept a replacement the ask cannot hold: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || after != before {
		t.Fatalf("document after the refused accept = %q (%v), want it unchanged: %q", after, err, before)
	}
	if _, err := documentService.VerifyMark(context.Background(), issue.PrimaryArtifactID, docs.MarkSuggestion, comment.Anchor.MarkID); err != nil {
		t.Fatalf("suggestion mark after the refused accept: %v, want it still in place", err)
	}
	stored := decodeBody[struct {
		Comment model.Comment `json:"comment"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/comments/"+comment.ID, nil, "alice")).Comment
	if stored.Resolved || stored.Suggestion == nil || stored.Suggestion.Accepted != nil {
		t.Fatalf("suggestion after the refused accept = %#v, want it open and unactioned", stored)
	}

	edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]any{{"op": "replace", "find": "Which one?", "with": replacement}},
	}, "alice")
	if edited.Code != http.StatusBadRequest || !strings.Contains(edited.Body.String(), `"code":"INVALID_OP"`) {
		t.Fatalf("the same text through the edit route: status=%d body=%s", edited.Code, edited.Body.String())
	}
	if after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || after != before {
		t.Fatalf("document after the refused edit = %q (%v), want it unchanged", after, err)
	}
}

// malformedAsk is an ask block whose body holds a code block. The server keeps one on purpose: a
// seeded spec or an upload carrying it is accepted, and a browser edit that makes one is stamped
// invalid, so a document can hold it for as long as nobody repairs it.
const malformedAsk = ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n\n```\ncode\n```\n:::\n"

// Only an ask an accept breaks is refused. A document already holding a malformed ask still takes
// a typo fix elsewhere, a whole-paragraph replacement, and the reject of a browser insert
// suggestion, as main did; and a reject is never refused for an ask, since it gives back the
// document the insert started from, even when the insert was an ask's whole question.
func TestSuggestionActionsAreRefusedOnlyForAnAskAnAcceptBroke(t *testing.T) {
	for _, test := range []struct {
		name, seed, upload, quote, replaceWith, action, want string
		browserInsert                                        bool
	}{
		{name: "an accept after an upload wrote the ask", seed: "Intro typo.\n", upload: "Intro typo.\n\n" + malformedAsk,
			quote: "typo", replaceWith: "fixed", action: "accept", want: "Intro fixed."},
		{name: "a block accept in a seeded document", seed: "Intro typo.\n\n" + malformedAsk,
			quote: "Intro typo.", replaceWith: "Intro fixed.\n\nMore.\n", action: "accept", want: "Intro fixed.\n\nMore."},
		{name: "a reject of a browser insert", seed: "The quick brown fox\n\n" + malformedAsk,
			quote: "quick ", action: "reject", want: "The brown fox", browserInsert: true},
		{name: "a reject that empties the question it inserted", seed: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n",
			quote: "Which one?", action: "reject", want: "Intro.", browserInsert: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "Beside a malformed ask", test.seed)
			if test.upload != "" {
				if uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
					"name": "spec.md", "content": test.upload,
				}, "alice"); uploaded.Code != http.StatusCreated {
					t.Fatalf("upload the malformed ask: status=%d body=%s", uploaded.Code, uploaded.Body.String())
				}
			}
			anchor := map[string]any{"artifact": "spec", "quote": test.quote}
			if test.browserInsert {
				writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
					Kind: docs.MarkSuggestion, ID: "m-insert", Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
				}, test.quote)
				anchor = map[string]any{"artifact": "spec", "mark_id": "m-insert"}
			}
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "suggested", "anchor": anchor, "suggestion": map[string]string{"replace_with": test.replaceWith},
			}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)

			acted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/"+test.action, map[string]any{}, "alice")

			if acted.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", test.action, acted.Code, acted.Body.String())
			}
			if text, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || !strings.HasPrefix(text, test.want+"\n") {
				t.Fatalf("document after the %s = %q (%v), want it to begin %q", test.action, text, err, test.want)
			}
		})
	}
}

// A replacement carrying an ask block under the id of an ask the document already holds would
// write two asks with one id, and the document's id repair keeps the id for the first in document
// order, so the new ask would take over the existing ask's row and its answer. The accept refuses
// it, and the document and the existing ask stay as they were.
func TestSuggestionAcceptRefusesAnAskUnderAnIdTheDocumentHolds(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "An ask under a held id",
		"Intro typo.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n")
	before, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "ask first", "anchor": map[string]any{"artifact": "spec", "quote": "Intro typo."},
		"suggestion": map[string]string{"replace_with": ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nOther?\n:::\n"},
		"actor":      sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)

	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

	if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), `"code":"INVALID_ASK_BLOCK"`) ||
		!strings.Contains(accepted.Body.String(), `duplicate ask block id \"a1\"`) {
		t.Fatalf("accept an ask under a held id: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || after != before {
		t.Fatalf("document after the refused accept = %q (%v), want it unchanged: %q", after, err, before)
	}
}

// A replacement no level of the document can hold where the suggestion sits, such as a code block
// over a table cell's whole text, is the caller's to fix: the accept is refused naming replace_with, and
// the document stays as it was, where the splice's schema error once reached the handler as a 500.
func TestSuggestionAcceptRefusesAReplacementTheDocumentCannotFit(t *testing.T) {
	var documentService *docs.Service
	handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Code over a table cell", "| head |\n| :--- |\n| a target c |\n")
	before, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "as code", "anchor": map[string]any{"artifact": "spec", "quote": "a target c"},
		"suggestion": map[string]string{"replace_with": "```\ncode\n```\n"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)

	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

	if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), `"code":"INVALID_OP"`) ||
		!strings.Contains(accepted.Body.String(), `\"replace_with\"`) {
		t.Fatalf("accept a code block over a table cell: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	if after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || after != before {
		t.Fatalf("document after the refused accept = %q (%v), want it unchanged: %q", after, err, before)
	}
}

func TestMarkAnchorMissingAfterWaitIs409(t *testing.T) {
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	issue := createInteractionIssue(t, handler, "TEST", "Missing browser mark", "The quick brown fox")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":   "why",
		"anchor": map[string]any{"artifact": "spec", "mark_id": "ghost"},
	}, "alice")
	if created.Code != http.StatusConflict || !strings.Contains(created.Body.String(), `"code":"ANCHOR_MISSING"`) {
		t.Fatalf("record missing browser mark: status=%d body=%s", created.Code, created.Body.String())
	}
	var comments int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, issue.Key).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}
	if comments != 0 {
		t.Fatalf("missing browser mark recorded %d comments", comments)
	}
}

func TestAnchorInputRequiresQuoteXorMarkID(t *testing.T) {
	tests := []struct {
		name   string
		anchor map[string]any
		code   string
	}{
		{name: "artifact only", anchor: map[string]any{"artifact": "spec"}, code: "INVALID_ANCHOR"},
		{name: "quote and mark id", anchor: map[string]any{"artifact": "spec", "quote": "brown", "mark_id": "m-1"}, code: "INVALID_ANCHOR"},
		{name: "empty quote", anchor: map[string]any{"artifact": "spec", "quote": ""}, code: "INVALID_ANCHOR"},
		{name: "legacy range", anchor: map[string]any{"artifact": "spec", "from": 0, "to": 3}, code: "INVALID_JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Invalid anchor", "The quick brown fox")
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
				"question": "Why?",
				"anchor":   test.anchor,
			}, "alice")
			if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"`+test.code+`"`) {
				t.Fatalf("invalid anchor: status=%d body=%s", created.Code, created.Body.String())
			}
		})
	}
}

func TestAnchorQuoteNotFoundIs404(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Missing quote", "The quick brown fox")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Why purple?",
		"anchor":   map[string]any{"artifact": "spec", "quote": "purple"},
	}, "alice")
	if created.Code != http.StatusNotFound || !strings.Contains(created.Body.String(), `"code":"TARGET_NOT_FOUND"`) {
		t.Fatalf("create missing quote anchor: status=%d body=%s", created.Code, created.Body.String())
	}
}

func TestReplyMayNotCarryAnAnchor(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored reply", "The quick brown fox")
	root := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": "root"}, "alice")
	if root.Code != http.StatusCreated {
		t.Fatalf("create root comment: status=%d body=%s", root.Code, root.Body.String())
	}
	comment := decodeBody[struct {
		ID string `json:"id"`
	}](t, root)
	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": "Why?"}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	askID := decodeBody[struct {
		ID string `json:"id"`
	}](t, ask).ID

	for _, body := range []map[string]any{
		{"body": "reply", "reply_to": comment.ID, "anchor": map[string]any{"artifact": "spec", "quote": "quick"}},
		{"body": "answer", "ask_id": askID, "anchor": map[string]any{"artifact": "spec", "quote": "quick"}},
	} {
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", body, "alice")
		if created.Code != http.StatusBadRequest || !strings.Contains(created.Body.String(), `"code":"INVALID_COMMENT"`) {
			t.Fatalf("anchored reply: status=%d body=%s", created.Code, created.Body.String())
		}
	}
}
