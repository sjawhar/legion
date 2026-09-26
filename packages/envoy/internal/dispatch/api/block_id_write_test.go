package api

import (
	"context"
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

// An id names one block. A write whose markdown carries the id of a block the document already
// holds would put a second block under it, and the id repair every write runs keeps the id for
// the first in document order: written ahead of the answered ask, a new ask would take its row and
// its answer, and a callout would leave the row on the callout or retract it; either way the
// question it answered would come back as a fresh open ask. Each write that can carry a typed
// block's id - an insert, an accepted suggestion, an uploaded version - is refused naming the id,
// and nothing changes.
func TestAWriteRepeatingAHeldBlockIDIsRefused(t *testing.T) {
	const callout = ":::callout{#decision}\nA note.\n:::\n"
	insert := func(markdown string) func(*testing.T, http.Handler, string, string) *httptest.ResponseRecorder {
		return func(t *testing.T, handler http.Handler, _, artifactID string) *httptest.ResponseRecorder {
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
				"ops": []map[string]string{{"op": "insert", "after": "start", "markdown": markdown}},
			}, "alice")
		}
	}
	for _, test := range []struct {
		name, code string
		write      func(t *testing.T, handler http.Handler, issueKey, artifactID string) *httptest.ResponseRecorder
	}{
		{name: "an inserted ask", code: "INVALID_OP",
			write: insert(":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich protocol?\n:::\n")},
		{name: "an inserted callout", code: "INVALID_OP", write: insert(callout)},
		{name: "an accepted suggestion", code: "INVALID_MARKDOWN",
			write: func(t *testing.T, handler http.Handler, issueKey, _ string) *httptest.ResponseRecorder {
				created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
					"body": "a note first", "anchor": map[string]any{"artifact": "spec", "quote": "Context"},
					"suggestion": map[string]string{"replace_with": callout}, "actor": sessionActor(),
				})
				if created.Code != http.StatusCreated {
					t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
				}
				comment := decodeBody[model.Comment](t, created)
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
			}},
		// The question alone is replaced, so the ask's options stay behind as the rest of the
		// block under its id: the id is held outside the text the accept replaces.
		{name: "an accepted rewording of the question alone", code: "INVALID_MARKDOWN",
			write: func(t *testing.T, handler http.Handler, issueKey, _ string) *httptest.ResponseRecorder {
				created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
					"body": "reword it", "anchor": map[string]any{"artifact": "spec", "quote": "Which transport?"},
					"suggestion": map[string]string{"replace_with": strings.Replace(transportAsk, "Which transport?", "Which transport, first?", 1)},
					"actor":      sessionActor(),
				})
				if created.Code != http.StatusCreated {
					t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
				}
				comment := decodeBody[model.Comment](t, created)
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
			}},
		{name: "an uploaded version", code: "INVALID_MARKDOWN",
			write: func(t *testing.T, handler http.Handler, issueKey, artifactID string) *httptest.ResponseRecorder {
				return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
					"name": "spec.md", "content": callout + "\n" + documentMarkdown(t, handler, artifactID),
				}, "alice")
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := blockAskHandler(t)
			issue, askID := seedBlockAsk(t, handler, "Held block id", "decision", transportAsk, "Which transport?")
			read := readBlockAsk(t, handler, askID)
			if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
				"selected": []string{"REST"}, "text": "REST first.", "expected_edited_at": read.EditedAt,
			}, "alice"); answered.Code != http.StatusOK {
				t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
			}
			before := documentMarkdown(t, handler, issue.PrimaryArtifactID)

			written := test.write(t, handler, issue.Key, issue.PrimaryArtifactID)

			if written.Code != http.StatusBadRequest || !strings.Contains(written.Body.String(), `"code":"`+test.code+`"`) ||
				!strings.Contains(written.Body.String(), `block id \"decision\" would name two blocks`) {
				t.Fatalf("%s repeating a held id: status=%d body=%s", test.name, written.Code, written.Body.String())
			}
			if after := documentMarkdown(t, handler, issue.PrimaryArtifactID); after != before {
				t.Fatalf("document after the refusal = %q, want it unchanged: %q", after, before)
			}
			settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-refusal")
			if ask := readBlockAsk(t, handler, askID); ask.Question != "Which transport?" || ask.State != "answered" {
				t.Fatalf("the answered ask after the refusal: question=%q state=%q", ask.Question, ask.State)
			}
			blockAskID(t, handler, issue.Key, "decision")
		})
	}
}

// An accepted suggestion that rewrites a typed block in place under its own id writes one block
// under that id, so it is taken as on main: the answered free-text ask keeps its row, its answer
// and its block, whether the suggestion quotes the ask's whole text or a span across it.
func TestAnAcceptRewritingATypedBlockUnderItsOwnIDIsTaken(t *testing.T) {
	const freeText = ":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich transport?\n:::\n"
	const reworded = ":::ask{#decision urgency=\"high\" multiple=\"false\"}\nWhich transport, first?\n:::\n"
	for _, test := range []struct{ name, quote string }{
		{name: "the ask's whole text", quote: "Which transport?"},
		{name: "a span across the ask", quote: "Context Which transport?"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := blockAskHandler(t)
			issue, askID := seedBlockAsk(t, handler, "Rewritten in place", "decision", freeText, "Which transport?")
			read := readBlockAsk(t, handler, askID)
			if answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{
				"selected": []string{}, "text": "REST first.", "expected_edited_at": read.EditedAt,
			}, "alice"); answered.Code != http.StatusOK {
				t.Fatalf("answer the ask: status=%d body=%s", answered.Code, answered.Body.String())
			}
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "reword it", "anchor": map[string]any{"artifact": "spec", "quote": test.quote},
				"suggestion": map[string]string{"replace_with": reworded}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)

			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

			if accepted.Code != http.StatusOK {
				t.Fatalf("accept rewriting the ask under its own id: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-accept")
			if got := strings.Count(documentMarkdown(t, handler, issue.PrimaryArtifactID), "{#decision "); got != 1 {
				t.Fatalf("the document holds %d blocks under decision, want 1", got)
			}
			if held := blockAskID(t, handler, issue.Key, "decision"); held != askID {
				t.Fatalf("block decision holds ask %s, want %s", held, askID)
			}
			if ask := readBlockAsk(t, handler, askID); ask.Question != "Which transport, first?" || ask.State != "answered" ||
				ask.Answer == nil || ask.Answer.Text == nil || *ask.Answer.Text != "REST first." {
				t.Fatalf("the answered ask after the accept: question=%q state=%q answer=%+v", ask.Question, ask.State, ask.Answer)
			}
		})
	}
}

// A block replacement anchored inside a paragraph splits it, and the splice gives both halves the
// paragraph's id until settlement's repair re-mints the second. That id is the document's, not
// one the caller wrote, so the accept is taken and stores what main does.
func TestABlockReplacementInsideAParagraphIsTaken(t *testing.T) {
	for _, test := range []struct{ name, replaceWith, want string }{
		{name: "two paragraphs", replaceWith: "one\n\ntwo\n", want: "Alpha \n\none\n\ntwo\n\n gamma.\n"},
		{name: "a callout", replaceWith: ":::callout{kind=\"note\"}\nA note.\n:::\n",
			want: "Alpha \n\n:::callout{#<minted> kind=\"note\" title=\"\"}\nA note.\n:::\n\n gamma.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := blockAskHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Split paragraph", "Alpha beta gamma.\n")
			created := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "split it", "anchor": map[string]any{"artifact": "spec", "quote": "beta"},
				"suggestion": map[string]string{"replace_with": test.replaceWith}, "actor": sessionActor(),
			})
			if created.Code != http.StatusCreated {
				t.Fatalf("create the suggestion: status=%d body=%s", created.Code, created.Body.String())
			}
			comment := decodeBody[model.Comment](t, created)

			accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")

			if accepted.Code != http.StatusOK {
				t.Fatalf("accept a block replacement inside a paragraph: status=%d body=%s", accepted.Code, accepted.Body.String())
			}
			minted := regexp.MustCompile(`#[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
			if got := minted.ReplaceAllString(documentMarkdown(t, handler, issue.PrimaryArtifactID), "#<minted>"); got != test.want {
				t.Fatalf("document after the accept = %q, want %q", got, test.want)
			}
		})
	}
}

// A spec seeded at issue creation that names one block id twice is refused the same way: the
// repair would hand the id to the first block and index the second as another ask.
func TestASeededSpecRepeatingABlockIDIsRefused(t *testing.T) {
	handler, _ := blockAskHandler(t)
	if created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "TEST project",
	}, "alice"); created.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", created.Code, created.Body.String())
	}

	seeded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Repeated id", "spec": ":::callout{#decision}\nA note.\n:::\n\n" + transportAsk,
	}, "alice")

	if seeded.Code != http.StatusBadRequest || !strings.Contains(seeded.Body.String(), `"code":"INVALID_MARKDOWN"`) ||
		!strings.Contains(seeded.Body.String(), `block id \"decision\" would name two blocks`) {
		t.Fatalf("seed a spec repeating an id: status=%d body=%s", seeded.Code, seeded.Body.String())
	}
}

// Markdown that names each block id at most once is written as before: a seed and an upload
// answer 201 and store what pmdoc.Parse renders, whether its typed blocks name no id (minted ids
// masked) or distinct ids of their own.
func TestAWriteNamingEachBlockIDOnceIsStoredAsBefore(t *testing.T) {
	minted := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	for _, test := range []struct{ name, markdown string }{
		{name: "no explicit ids", markdown: "# Plan\n\nSome text.\n\n:::callout{kind=\"note\"}\nA note.\n:::\n\n" +
			":::ask{urgency=\"med\" multiple=\"false\"}\nWhich one?\n\n- A\n- B\n:::\n\n- one\n- two\n"},
		{name: "distinct explicit ids", markdown: "# Plan\n\n:::callout{#note-1}\nA note.\n:::\n\n" +
			":::ask{#q-1 urgency=\"med\" multiple=\"false\"}\nWhich one?\n:::\n\n:::callout{#note-2 kind=\"warning\"}\nCareful.\n:::\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := pmdoc.Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			want, err := pmdoc.Render(parsed)
			if err != nil {
				t.Fatal(err)
			}
			handler, _ := newInteractionHandler(t, func(database *store.Store) docs.API {
				documentService := docs.New(docs.Deps{Store: database, Settle: time.Hour, MarkWait: 50 * time.Millisecond})
				t.Cleanup(func() { _ = documentService.Shutdown(context.Background()) })
				return documentService
			})

			seeded := createInteractionIssue(t, handler, "TEST", "Seeded once", test.markdown)
			if got := documentMarkdown(t, handler, seeded.PrimaryArtifactID); minted.ReplaceAllString(got, "<minted>") != minted.ReplaceAllString(want, "<minted>") {
				t.Fatalf("seeded spec = %q, want %q", got, want)
			}
			uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+seeded.Key+"/artifacts", map[string]any{
				"name": "notes.md", "content": test.markdown,
			}, "alice")
			if uploaded.Code != http.StatusCreated {
				t.Fatalf("upload: status=%d body=%s", uploaded.Code, uploaded.Body.String())
			}
			artifact := decodeBody[struct {
				Artifact model.Artifact `json:"artifact"`
			}](t, uploaded).Artifact
			if got := documentMarkdown(t, handler, artifact.ID); minted.ReplaceAllString(got, "<minted>") != minted.ReplaceAllString(want, "<minted>") {
				t.Fatalf("uploaded document = %q, want %q", got, want)
			}
		})
	}
}
