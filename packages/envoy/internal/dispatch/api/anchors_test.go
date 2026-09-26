package api

import (
	"context"
	"errors"

	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/identity"
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

// malformedAsk is an ask block whose body holds a code block. No route writes one, but a browser
// edit that makes one is kept and stamped invalid, and an upload can carry it on unchanged, so a
// document can hold it for as long as nobody repairs it.
const malformedAsk = ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n\n```\ncode\n```\n:::\n"

// browserDocumentService is a document service a browser peer can connect to (writeBrowserDocument).
func browserDocumentService(t *testing.T) (*docs.Service, http.Handler, *store.Store) {
	t.Helper()
	var documentService *docs.Service
	handler, database := newInteractionHandler(t, func(database *store.Store) docs.API {
		documentService = docs.New(docs.Deps{
			Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond,
			Identity: identity.HeaderIdentity{Header: "X-Dispatch-User", AllowedLogins: map[string]struct{}{"alice": {}}},
		})
		t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
		return documentService
	})
	return documentService, handler, database
}

// writeBrowserDocument stands in for a browser editor that turns the live document into
// markdown's tree. A browser's update is not held to the checks the server's own writes run, which
// is how a document comes to hold what no route writes, such as a malformed ask. The peer stays
// connected for the rest of the test, as an open editor does, so the room stays resident.
func writeBrowserDocument(t *testing.T, documentService *docs.Service, artifactID, markdown string) {
	t.Helper()
	written, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the browser's document: %v", err)
	}
	sockets := &servedSockets{finished: make(map[string]chan struct{})}
	server := httptest.NewServer(sockets.serve(documentService.ServeHTTP))
	t.Cleanup(server.Close)
	peer := &syncedPeer{
		wsURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID, sockets: sockets,
		headers: http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID: artifactID, doc: crdt.New(),
	}
	peer.connect(t)
	t.Cleanup(peer.close)
	peer.edit(t, func(tree *pmdoc.Node) error {
		tree.Children = written.Children
		return nil
	})
	rendered, err := pmdoc.Render(written)
	if err != nil {
		t.Fatalf("render the browser's document: %v", err)
	}
	waitForLiveText(t, documentService, artifactID, rendered)
}

// Only an ask an accept breaks is refused. A document already holding a malformed ask still takes
// a typo fix elsewhere, a whole-paragraph replacement, and the reject of a browser insert
// suggestion, as main did; and a reject is never refused for an ask, since it gives back the
// document the insert started from, even when the insert was an ask's whole question.
func TestSuggestionActionsAreRefusedOnlyForAnAskAnAcceptBroke(t *testing.T) {
	for _, test := range []struct {
		name, seed, browser, quote, replaceWith, action, want string
		browserInsert                                         bool
	}{
		{name: "a typo fix beside the ask", seed: "Intro typo.\n", browser: "Intro typo.\n\n" + malformedAsk,
			quote: "typo", replaceWith: "fixed", action: "accept", want: "Intro fixed."},
		{name: "a block accept beside the ask", seed: "Intro typo.\n", browser: "Intro typo.\n\n" + malformedAsk,
			quote: "Intro typo.", replaceWith: "Intro fixed.\n\nMore.\n", action: "accept", want: "Intro fixed.\n\nMore."},
		{name: "a reject of a browser insert", seed: "The quick brown fox\n", browser: "The quick brown fox\n\n" + malformedAsk,
			quote: "quick ", action: "reject", want: "The brown fox", browserInsert: true},
		{name: "a typo fix inside an ask already malformed", seed: "Intro.\n", browser: "Intro.\n\n" + malformedAsk,
			quote: "one", replaceWith: "two", action: "accept", want: "Which two?"},
		{name: "a reject that empties the question it inserted", seed: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n",
			quote: "Which one?", action: "reject", want: "Intro.", browserInsert: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Beside a malformed ask", test.seed)
			if test.browser != "" {
				writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, test.browser)
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
			if text, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || !strings.Contains(text, test.want) {
				t.Fatalf("document after the %s = %q (%v), want it to hold %q", test.action, text, err, test.want)
			}
		})
	}
}

// A block replacement over a callout's only paragraph lands inside the callout, as ProseMirror's
// fit puts it there, so the callout keeps its id, kind and title whatever the blocks are. The fit
// once climbed out of the callout and replaced it with the blocks, which both parsers read back as
// written, so nothing downstream could notice the callout was gone.
func TestSuggestionAcceptKeepsTheCalloutItLandsIn(t *testing.T) {
	for _, test := range []struct{ name, replaceWith, body string }{
		{name: "a rule", replaceWith: "***\n", body: "---\n"},
		{name: "a list", replaceWith: "- a\n", body: "- a\n"},
		{name: "a heading", replaceWith: "# h\n", body: "# h\n"},
		{name: "a blockquote", replaceWith: "> q\n", body: "> q\n"},
		{name: "a code block", replaceWith: "```\ncode\n```\n", body: "```\ncode\n```\n"},
		{name: "two paragraphs", replaceWith: "a\n\nb\n", body: "a\n\nb\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "A callout's only paragraph",
				"Intro.\n\n:::callout{#c1 kind=\"warning\" title=\"T\"}\nOnly.\n:::\n")
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "suggested", "anchor": map[string]any{"artifact": "spec", "quote": "Only."},
				"suggestion": map[string]string{"replace_with": test.replaceWith}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)

			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

			if accepted.Code != http.StatusOK {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			want := "Intro.\n\n:::callout{#c1 kind=\"warning\" title=\"T\"}\n" + test.body + ":::\n"
			if text, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || text != want {
				t.Fatalf("document after the accept = %q (%v), want %q", text, err, want)
			}
		})
	}
}

// Two paragraphs over a word of an answered ask's question land inside the ask, as ProseMirror's
// fit puts them there: the ask's content rule allows a question of several paragraphs. The ask
// keeps its id, its options, its row and its answer. The fit once climbed out of the ask and split
// it, leaving the answered row on "Which" and the options under a minted id as a new open ask "?".
func TestSuggestionAcceptKeepsTheAskItLandsIn(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue, askID := seedBlockAsk(t, handler, "Two paragraphs in a question", "decision", transportAsk, "Which transport?")
	read := readBlockAsk(t, handler, askID)
	if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
		"selected": []string{"REST"}, "text": "REST first.", "expected_edited_at": read.EditedAt,
	}, "alice"); answered.Code != http.StatusOK {
		t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
	}
	created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "suggested", "anchor": map[string]any{"artifact": "spec", "quote": "transport"},
		"suggestion": map[string]string{"replace_with": "one\n\ntwo\n"}, "actor": sessionActor(),
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)

	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

	if accepted.Code != http.StatusOK {
		t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-accept")
	asks := decodeBody[[]model.Ask](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks?state=all", nil, "alice"))
	if len(asks) != 2 {
		t.Fatalf("asks after the accept = %+v, want the answered ask and settleDocument's marker", asks)
	}
	ask := readBlockAsk(t, handler, askID)
	if ask.BlockID == nil || *ask.BlockID != "decision" || ask.State != "answered" || len(ask.Options) != 2 ||
		ask.Question != "Which\n\none\n\ntwo\n\n?" {
		t.Fatalf("the answered ask after the accept: block=%v state=%q options=%d question=%q",
			ask.BlockID, ask.State, len(ask.Options), ask.Question)
	}
}

// Only the typed block itself, rewritten under its own id, is replaced by a replacement of its
// type. One under no id is a new block, so it lands inside the callout as any block does, and the
// callout keeps its id; the inner callout of two, rewritten under its own id, is replaced where it
// sits, inside the outer one.
func TestSuggestionAcceptRewritesOnlyTheTypedBlockUnderItsOwnID(t *testing.T) {
	minted := regexp.MustCompile(`#[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	for _, test := range []struct{ name, spec, quote, replaceWith, want string }{
		{name: "a callout under no id", spec: "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"\"}\nA note.\n:::\n",
			quote: "A note.", replaceWith: ":::callout{kind=\"note\"}\nReworded.\n:::\n",
			want: "Intro.\n\n:::callout{#c1 kind=\"note\" title=\"\"}\n:::callout{#<minted> kind=\"note\" title=\"\"}\nReworded.\n:::\n:::\n"},
		{name: "the inner of two callouts under its own id",
			spec:  ":::callout{#outer kind=\"note\" title=\"\"}\nOuter.\n\n:::callout{#inner kind=\"note\" title=\"\"}\nWhich one?\n:::\n",
			quote: "Which one?", replaceWith: ":::callout{#inner kind=\"warning\" title=\"\"}\nReworded.\n:::\n",
			want: ":::callout{#outer kind=\"note\" title=\"\"}\nOuter.\n\n:::callout{#inner kind=\"warning\" title=\"\"}\nReworded.\n:::\n:::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var documentService *docs.Service
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService = docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})
			issue := createInteractionIssue(t, handler, "TEST", "A typed block rewritten", test.spec)
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "suggested", "anchor": map[string]any{"artifact": "spec", "quote": test.quote},
				"suggestion": map[string]string{"replace_with": test.replaceWith}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)

			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

			if accepted.Code != http.StatusOK {
				t.Fatalf("accept: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			text, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
			if got := minted.ReplaceAllString(text, "#<minted>"); err != nil || got != test.want {
				t.Fatalf("document after the accept = %q (%v), want %q", got, err, test.want)
			}
		})
	}
}

// askSpec is a document holding one open ask, a1, between two paragraphs.
const askSpec = "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\nAfter.\n"

// Every accept refused for what it would write leaves the document byte-identical, the
// suggestion's mark in place, and the comment open and unactioned.
//   - An ask holds paragraphs and at most one bullet list. A question given a code block would leave
//     an ask settlement cannot read; the splice once fitted it by replacing the whole block, so the
//     question someone may be waiting on vanished. It is refused with settlement's reason, alone or
//     beside another ask that was already malformed (that one is not the accept's, the ask it
//     breaks is), and so is the same text through the edit route.
//   - A replacement that leaves a paragraph after an ask's options, or a second bullet list in
//     it, breaks the ask's content rule, paragraph+ bullet_list?; settlement's parse takes it, but
//     the browser editor drops such an ask from the shared document when it renders it, and
//     settlement then retracts it.
//   - Deleting an ask's whole question leaves an ask with no question.
//   - A replacement carrying an ask under a1's id would write two asks with one id, and the id
//     repair keeps the id for the first in document order, handing a1's row and answer to the new
//     ask; the write's block-id check refuses it before the ask check runs.
//   - A code block over a table cell's whole text fits nowhere; the splice's schema error once
//     reached the handler as a 500.
//   - Inline text over a range that runs into an ask or callout from the text before it, at any
//     depth (in a blockquote, a list item, a callout), would join the two and leave that block
//     empty (the engine drops it, which for an ask retracts it); the splice's schema error once
//     reached the handler as a 500.
func TestSuggestionAcceptRefusals(t *testing.T) {
	codeQuestion := "Which?\n\n```\ncode\n```\n"
	for _, test := range []struct {
		name, spec, browser, quote, replaceWith, code, reason string
		sameThroughEdits                                      bool
	}{
		{name: "a question given a code block", spec: askSpec, quote: "Which one?", replaceWith: codeQuestion,
			code: "INVALID_ASK_BLOCK", reason: `ask block \"a1\" has unsupported body node \"code_block\"`, sameThroughEdits: true},
		{name: "a question given a code block beside a malformed ask", quote: "Which one?", replaceWith: codeQuestion, spec: askSpec,
			browser: askSpec + "\n" + strings.NewReplacer("#a1", "#a2", "Which one?", "Ship it?").Replace(malformedAsk),
			code:    "INVALID_ASK_BLOCK", reason: `ask block \"a1\" has unsupported body node \"code_block\"`, sameThroughEdits: true},
		{name: "a paragraph after a free-text ask's new options", spec: askSpec, quote: "Which one?",
			replaceWith: "Which database?\n\n- Postgres\n- SQLite\n\nPick one by Friday.\n",
			code:        "INVALID_ASK_BLOCK", reason: `holds a paragraph after its options`},
		{name: "a second bullet list in an ask with options", quote: "Which one?", replaceWith: "Which?\n\n- X\n",
			spec: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n\n- A\n- B\n:::\n",
			code: "INVALID_ASK_BLOCK", reason: `holds a second bullet list`},
		{name: "an ask's only question replaced by a heading", spec: askSpec, quote: "Which one?", replaceWith: "# h\n",
			code: "INVALID_ASK_BLOCK", reason: `ask block \"a1\" has unsupported body node \"heading\"`},
		{name: "an ask's whole question deleted", spec: askSpec, quote: "Which one?", replaceWith: "",
			code: "INVALID_ASK_BLOCK", reason: `ask block \"a1\" has an empty question`},
		{name: "an ask under a held id", spec: "Intro typo.\n\n" + askSpec, quote: "Intro typo.",
			replaceWith: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nOther?\n:::\n",
			code:        "INVALID_MARKDOWN", reason: `block id \"a1\" would name two blocks`},
		{name: "an ask under an id held malformed", spec: "Intro typo.\n", browser: "Intro typo.\n\n" + malformedAsk, quote: "Intro typo.",
			replaceWith: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nOther?\n:::\n",
			code:        "INVALID_MARKDOWN", reason: `block id \"a1\" would name two blocks`},
		{name: "an ask rewritten under another id", spec: askSpec, quote: "Which one?",
			replaceWith: ":::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one, first?\n:::\n",
			code:        "INVALID_ASK_BLOCK", reason: `ask block \"a1\" has unsupported body node \"ask\"`},
		{name: "an ask rewritten under no id", spec: askSpec, quote: "Which one?",
			replaceWith: ":::ask{urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one, first?\n:::\n",
			code:        "INVALID_ASK_BLOCK", reason: `ask block \"a1\" has unsupported body node \"ask\"`},
		{name: "text from one ask's question into the next's", spec: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n" +
			":::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n",
			quote: "one? Ship", replaceWith: "x", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one ask's question into the next's, deleted", spec: "Intro.\n\n:::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n" +
			":::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n",
			quote: "one? Ship", replaceWith: "", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one callout into the next", spec: ":::callout{#c1}\nWhich one?\n:::\n\n:::callout{#c2}\nShip it?\n:::\n",
			quote: "one? Ship", replaceWith: "x", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one ask into the next inside a blockquote", quote: "one? Ship", replaceWith: "x",
			spec: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n" + "> :::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\n> Ship it?\n> :::\n", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one ask into the next inside a list item", quote: "one? Lead. Ship", replaceWith: "x",
			spec: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n" + "- Lead.\n\n  :::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\n  Ship it?\n  :::\n", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one callout into the next inside a blockquote", quote: "one? Ship", replaceWith: "x",
			spec: ":::callout{#c1}\nWhich one?\n:::\n\n> :::callout{#c2}\n> Ship it?\n> :::\n", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one ask into an ask that is a callout's only content", quote: "one? Ship", replaceWith: "x",
			spec: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n" + ":::callout{#c2}\n:::ask{#a2 urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n:::\n", code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "text from one ask into a callout nested in a list item", quote: "one? Lead. Ship", replaceWith: "x",
			spec: ":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich one?\n:::\n\n- Lead.\n\n  :::callout{#c2}\n  Ship it?\n  :::\n",
			code: "INVALID_OP", reason: `field \"anchor\"`},
		{name: "a code block over a table cell's whole text", spec: "| head |\n| :--- |\n| a target c |\n", quote: "a target c",
			replaceWith: "```\ncode\n```\n", code: "INVALID_OP", reason: `field \"replace_with\"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, _ := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "A refused accept", test.spec)
			if test.browser != "" {
				writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, test.browser)
			}
			before, err := documentService.Text(context.Background(), issue.PrimaryArtifactID)
			if err != nil {
				t.Fatal(err)
			}
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "suggested", "anchor": map[string]any{"artifact": "spec", "quote": test.quote},
				"suggestion": map[string]string{"replace_with": test.replaceWith}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)

			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

			if accepted.Code != http.StatusBadRequest || !strings.Contains(accepted.Body.String(), `"code":"`+test.code+`"`) ||
				!strings.Contains(accepted.Body.String(), test.reason) {
				t.Fatalf("accept: status=%d body=%s, want 400 %s naming %s", accepted.Code, accepted.Body.String(), test.code, test.reason)
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
			if !test.sameThroughEdits {
				return
			}
			edited := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
				"ops": []map[string]any{{"op": "replace", "find": test.quote, "with": test.replaceWith}},
			}, "alice")
			if edited.Code != http.StatusBadRequest || !strings.Contains(edited.Body.String(), `"code":"INVALID_OP"`) {
				t.Fatalf("the same text through the edit route: status=%d body=%s", edited.Code, edited.Body.String())
			}
			if after, err := documentService.Text(context.Background(), issue.PrimaryArtifactID); err != nil || after != before {
				t.Fatalf("document after the refused edit = %q (%v), want it unchanged", after, err)
			}
		})
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
