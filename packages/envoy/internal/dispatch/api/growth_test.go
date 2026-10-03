package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Writes add up, so a write may not leave a document's markdown past what one upload may hold, by
// an upload's own measures - 1 MiB, and 65,536 elements as an upload's parse counts them - and
// bigger than it was. Inserts each well within both are refused with 413 CAP_EXCEEDED, saying what
// to do, once the document would pass either, whatever makes it grow: prose and a code block's text
// by their bytes, `)_`, headings, hard breaks, list items and table rows by their elements. The
// document still reads, its text and its blocks, and whatever the inserts left uploads back from its
// own text. Without the bound, thirty-two 900 KB inserts of prose grew one document to 29.5 MB, and
// four to six `)_` inserts or eighteen of headings left one the server could no longer load: the
// next insert answered 500, and its reads 503.
func TestRepeatedInsertsCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	tableSpec := "| a | b |\n| - | - |\n| A10 | b |\n"
	for _, test := range []struct {
		name, spec, after, chunk string
		inserts                  int
	}{
		{"900 KB of prose", "Before.\n", "end", strings.Repeat("word ", 180_000), 3},
		{"900 KB of a code block's text", "Before.\n", "end", "```\n" + strings.Repeat("a line of code, forty bytes long; more\n", 22_500) + "```\n", 3},
		{"42 KB of )_", "Before.\n", "end", strings.Repeat(")_", 21_000), 6},
		{"a thousand headings", "Before.\n", "end", strings.Repeat("# a\n", 1_000), 20},
		{"two thousand hard breaks", "Before.\n", "end", strings.Repeat("a  \n", 2_000) + "a\n", 12},
		{"three thousand list items", "Before.\n", "end", strings.Repeat("- a\n", 3_000), 8},
		{"a thousand table rows", tableSpec, "A10", strings.Repeat("| x | y |\n", 1_000), 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestHandler(t)
			issue := createInteractionIssue(t, handler, "TEST", "Repeated growth", test.spec)
			refused := 0
			for index := range test.inserts {
				response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": test.after, "markdown": test.chunk}},
				}, "alice")
				if response.Code == http.StatusOK {
					continue
				}
				if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) ||
					!strings.Contains(response.Body.String(), "shorten the change, or split the document") {
					t.Fatalf("insert %d: status=%d body=%.500s, want 200 or 413 CAP_EXCEEDED saying to shorten the change", index+1, response.Code, response.Body.String())
				}
				refused = index + 1
				t.Logf("insert %d refused: %.300s", refused, response.Body.String())
				break
			}
			if refused == 0 {
				t.Fatalf("all %d inserts were taken, want the document's growth refused", test.inserts)
			}
			for _, read := range []string{"/text", "/blocks"} {
				if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
					t.Fatalf("%s after insert %d was refused: status=%d body=%.300s, want 200", read, refused, response.Code, response.Body.String())
				}
			}
			text := documentMarkdown(t, handler, issue.PrimaryArtifactID)
			uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "spec.md"}, "spec.md", "text/markdown", []byte(text), "alice")
			if uploaded.Code != http.StatusCreated {
				t.Fatalf("upload of the document's own %d-byte text: status=%d body=%.500s, want 201", len(text), uploaded.Code, uploaded.Body.String())
			}
		})
	}
}

// A new document is measured as an upload is: front matter, which an upload's parse does not
// count, beside 16,384 headings, which weigh the 65,536 elements one document may hold, is a spec
// the server takes.
func TestASpecOfFrontMatterAndTheMostHeadingsIsTaken(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Front matter", "---\ntitle: a spec\n---\n\n"+strings.Repeat("# a\n", 16_384))
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); !strings.HasPrefix(text, "---\ntitle: a spec\n---\n") || strings.Count(text, "# a\n") != 16_384 {
		t.Fatalf("the spec reads back %d bytes opening %q, want its front matter and 16,384 headings", len(text), text[:min(len(text), 40)])
	}
}

// A new document is stored as its rendering, which can run longer than the markdown sent - the
// renderer writes a blank line between two headings - so the rendering is measured as well: one
// upload of exactly 1 MiB of 16,384 headings, which would be stored as 1,064,959 bytes, is refused
// with 413 CAP_EXCEEDED naming the cap and creates nothing, where the same headings two bytes
// shorter, whose rendering fits, are stored.
func TestANewDocumentWhoseRenderingPassesTheCapIsRefused(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	headings := func(width int) []byte {
		return []byte(strings.Repeat("# "+strings.Repeat("a", width-3)+"\n", 16_384))
	}
	upload := func(name string, markdown []byte) *httptest.ResponseRecorder {
		return multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": name}, "body.md", "text/markdown", markdown, "alice")
	}
	if markdown := headings(64); len(markdown) != 1<<20 {
		t.Fatalf("the headings are %d bytes, want exactly 1 MiB", len(markdown))
	}
	if response := upload("long.md", headings(64)); !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "a markdown document is at most 1 MiB (1048576 bytes), and this change would make the document's markdown 1064959 bytes") {
		t.Fatalf("a new document whose rendering passes 1 MiB: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the cap and the rendering's 1,064,959 bytes", response.Code, response.Body.String())
	}
	if listing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts", nil, "alice"); strings.Contains(listing.Body.String(), "long.md") {
		t.Fatalf("the refused document left an artifact behind: %.300s", listing.Body.String())
	}
	if response := upload("short.md", headings(62)); response.Code != http.StatusCreated {
		t.Fatalf("a new document whose rendering fits: status=%d body=%.400s, want 201", response.Code, response.Body.String())
	}
}

// An accepted suggestion adds the caller's text as an edit does: on a document that weighs what one
// document may hold, an accept that would make it heavier is refused with 413 CAP_EXCEEDED, saying
// to shorten the change, and the document keeps its text.
func TestAnAcceptCannotGrowADocumentPastWhatOneDocumentMayHold(t *testing.T) {
	handler := newTestHandler(t)
	// 16,384 headings of four elements each: the 65,536 one document may hold.
	issue := createInteractionIssue(t, handler, "TEST", "Accepted growth", strings.Repeat("# a\n", 16_383)+"# target\n")
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body":       "more",
		"anchor":     map[string]any{"artifact": "spec", "quote": "target"},
		"suggestion": map[string]string{"replace_with": "target **and more**"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("suggest: status=%d body=%.300s", created.Code, created.Body.String())
	}
	comment := decodeBody[model.Comment](t, created)
	accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice")
	if accepted.Code != http.StatusRequestEntityTooLarge || !strings.Contains(accepted.Body.String(), `"code":"CAP_EXCEEDED"`) ||
		!strings.Contains(accepted.Body.String(), "shorten the change, or split the document") {
		t.Fatalf("accept: status=%d body=%.500s, want 413 CAP_EXCEEDED saying to shorten the change", accepted.Code, accepted.Body.String())
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); !strings.HasSuffix(text, "# target\n") {
		t.Fatalf("the document after the refused accept ends %q, want its own last heading", text[max(0, len(text)-40):])
	}
}

// An ask's text is written into its block, so it is caller text in the document as an edit's is,
// and editing the asks of one document cannot grow it past what one upload may hold either:
// through this route thirty-two option descriptions of 900 KB grew one document to 28.8 MB, on
// which a one-word edit then took the server past the production task's 1,024 MiB. The edit that
// would pass the bound is refused with 413 CAP_EXCEEDED, saying what to do; the ask keeps its text
// and stays open, and the document still reads.
func TestAskEditsCannotGrowADocumentPastWhatOneUploadMayHold(t *testing.T) {
	prose := strings.Repeat("word ", 180_000)
	handler, _ := blockAskHandler(t)
	const asks = 3
	issue := createInteractionIssue(t, handler, "TEST", "Ask growth", openAsksSpec(asks))
	refused := 0
	for index := range asks {
		blockID := fmt.Sprintf("ask-%d", index)
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, blockID, fmt.Sprintf("Question %d?", index))
		askID := blockAskID(t, handler, issue.Key, blockID)
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
			"options": []map[string]string{{"label": "A", "description": prose}},
		}, "alice")
		if response.Code == http.StatusOK && refused == 0 {
			continue
		}
		if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "shorten the change, or split the document") {
			t.Fatalf("edit %d: status=%d body=%.500s, want 200 until one is refused with 413 CAP_EXCEEDED saying to shorten the change, as the document service words it, and 413 after it", index+1, response.Code, response.Body.String())
		}
		if ask := readBlockAsk(t, handler, askID); ask.State != "open" || len(ask.Options) != 0 {
			t.Fatalf("the ask after its refused edit = %#v, want it as it was", ask)
		}
		if refused == 0 {
			refused = index + 1
			t.Logf("edit %d refused: %.300s", refused, response.Body.String())
		}
	}
	if refused == 0 {
		t.Fatalf("all %d edits were taken, want the document's growth refused", asks)
	}
	for _, read := range []string{"/text", "/blocks"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s after edit %d was refused: status=%d body=%.300s, want 200", read, refused, response.Code, response.Body.String())
		}
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); len(text) > 1<<20 {
		t.Fatalf("the document's markdown is %d bytes, past the 1 MiB one upload may hold", len(text))
	}
}

// An ask's question and options are plain text the server makes into nodes, renders escaped and
// reads back, so they are weighed before that, as caller markdown is weighed while goldmark reads
// it: an option description whose syntax characters or line feeds alone weigh more elements than
// one write may make is refused with 413 CAP_EXCEEDED, saying so, before it is rendered, and the
// refusal allocates a fraction of what rendering it took. A 920 KB description of `)_` held 417 MiB
// before its edit was weighed, 400 KB of it was taken, and one of 330,000 lines did not finish its
// edit in twenty minutes. An ordinary long description is taken.
func TestAnAsksTextIsWeighedBeforeItIsRendered(t *testing.T) {
	handler, _ := blockAskHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Ask text", openAsksSpec(1))
	awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-0", "Question 0?")
	askID := blockAskID(t, handler, issue.Key, "ask-0")
	edit := func(description string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+askID, map[string]any{
			"options": []map[string]string{{"label": "A", "description": description}},
		}, "alice")
	}
	for _, description := range []struct{ name, text string }{
		{"400 KB of )_", strings.Repeat(")_", 200_000)},
		{"40,000 lines", strings.Repeat("a\n", 40_000)},
	} {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		response := edit(description.text)
		runtime.ReadMemStats(&after)
		if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "this write's text weighs at least") {
			t.Fatalf("an option description of %s: status=%d body=%.500s, want 413 CAP_EXCEEDED refusing the text's weight", description.name, response.Code, response.Body.String())
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 160<<20 {
			t.Errorf("refusing an option description of %s allocated %d MiB, want at most 160 MiB", description.name, allocated>>20)
		}
		if ask := readBlockAsk(t, handler, askID); ask.State != "open" || len(ask.Options) != 0 {
			t.Fatalf("the ask after its refused edit = %#v, want it as it was", ask)
		}
	}
	if response := edit(strings.Repeat("word, ", 100_000)); response.Code != http.StatusOK {
		t.Fatalf("an option description of 600 KB of prose: status=%d body=%.300s, want 200", response.Code, response.Body.String())
	}
}

// An answer is written into its ask's block, and the answer itself - its words, and the options it
// selects, whose labels are the asker's text and render escaped, a `>` as seven bytes - is caller
// text. Where the document has no room for it the ask still takes the answer whole, and its block
// is written without it, saying who answered and when, so no answer grows the document past what
// one upload may hold: one choice of a 1,000,000-character option took a 1 MB document to 8 MB,
// and an answer of 900 KB to a document with no room for it was refused and recorded nowhere.
func TestAnAnswerTheDocumentHasNoRoomForIsKeptOnItsAskAndLeftOutOfItsBlock(t *testing.T) {
	t.Run("words", func(t *testing.T) {
		prose := strings.Repeat("word ", 180_000)
		handler, _ := blockAskHandler(t)
		const asks = 3
		issue := createInteractionIssue(t, handler, "TEST", "Answer growth", openAsksSpec(asks))
		for index := range asks {
			blockID := fmt.Sprintf("ask-%d", index)
			awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, blockID, fmt.Sprintf("Question %d?", index))
			askID := blockAskID(t, handler, issue.Key, blockID)
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{"text": prose}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("answer %d: status=%d body=%.300s, want 200", index+1, response.Code, response.Body.String())
			}
			if ask := readBlockAsk(t, handler, askID); ask.State != "answered" || ask.Answer == nil || ask.Answer.Text == nil || *ask.Answer.Text != prose {
				t.Fatalf("ask %d after its answer = %#v, want it answered with the whole text", index+1, ask)
			}
		}
		text := documentMarkdown(t, handler, issue.PrimaryArtifactID)
		if len(text) > 1<<20 {
			t.Fatalf("the answers left a %d-byte document, past the 1 MiB one upload may hold", len(text))
		}
		if line := askDirective(t, text, "ask-0"); !strings.Contains(line, `answer="`+prose) {
			t.Fatalf("the block of the answer the document had room for reads %.300s, want the answer", line)
		}
		for _, blockID := range []string{"ask-1", "ask-2"} {
			if line := askDirective(t, text, blockID); !strings.Contains(line, `state="answered" answered_by="alice" answered_at="`) || strings.Contains(line, "answer=") {
				t.Fatalf("the block of an answer the document had no room for reads %.300s, want who answered and when, without the answer", line)
			}
		}
	})
	t.Run("a choice of a 1,000,000-character option", func(t *testing.T) {
		handler, _ := blockAskHandler(t)
		label := "a" + strings.Repeat(">", 1_000_000)
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "TEST", "name": "TEST project"}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
		}
		created := literalRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": "TEST", "title": "A long option",
			"spec": ":::ask{#ask-0 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich?\n\n- " + label + "\n:::\n",
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create issue: status=%d body=%.300s", created.Code, created.Body.String())
		}
		issue := decodeBody[struct {
			Key               string `json:"key"`
			PrimaryArtifactID string `json:"primary_artifact_id"`
		}](t, created)
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, "ask-0", "Which?")
		askID := blockAskID(t, handler, issue.Key, "ask-0")
		if response := literalRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]any{"selected": []string{label}, "expected_edited_at": nil}); response.Code != http.StatusOK {
			t.Fatalf("answer with the long option: status=%d body=%.300s, want 200", response.Code, response.Body.String())
		}
		if ask := readBlockAsk(t, handler, askID); ask.State != "answered" || ask.Answer == nil || len(ask.Answer.Selected) != 1 || ask.Answer.Selected[0] != label {
			t.Fatalf("the ask after its answer = %.300v, want it answered with the long option", ask)
		}
		text := documentMarkdown(t, handler, issue.PrimaryArtifactID)
		if len(text) > 1<<20 {
			t.Fatalf("one choice left a %d-byte document, past the 1 MiB one upload may hold", len(text))
		}
		if line := askDirective(t, text, "ask-0"); !strings.Contains(line, `state="answered" answered_by="alice" answered_at="`) || strings.Contains(line, "selected=") {
			t.Fatalf("the block of the choice reads %.300s, want who answered and when, without the selection", line)
		}
	})
}

// openAsksSpec is a spec of asks open ask blocks, ask-0 asking "Question 0?" and on.
func openAsksSpec(asks int) string {
	var spec strings.Builder
	spec.WriteString("Context\n")
	for index := range asks {
		fmt.Fprintf(&spec, "\n:::ask{#ask-%d urgency=\"med\" multiple=\"false\" state=\"open\"}\nQuestion %d?\n:::\n", index, index)
	}
	return spec.String()
}

// askDirective is the directive line of the ask block blockID in markdown.
func askDirective(t *testing.T, markdown, blockID string) string {
	t.Helper()
	for line := range strings.SplitSeq(markdown, "\n") {
		if strings.HasPrefix(line, ":::ask{#"+blockID+" ") || strings.HasPrefix(line, ":::ask{#"+blockID+"}") {
			return line
		}
	}
	t.Fatalf("the document holds no %s block:\n%.500s", blockID, markdown)
	return ""
}

// literalRequest is dispatchRequest with the body encoded as a browser or curl sends it, `<`, `>`
// and `&` as themselves, where Go's \u003e would take a long text past the 1 MiB request cap.
func literalRequest(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		t.Fatalf("encode request body: %v", err)
	}
	request := httptest.NewRequest(method, target, &encoded)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// A suggestion's replacement and a comment's text live in the document's margin, which every load
// of the document builds, so they are weighed as the document is: a comment's record holds at
// most 256 KiB of text and the margin 1 MiB, and a comment that would pass either is refused with
// 413 CAP_EXCEEDED naming the limit and both sizes, with the document as it was. Ordinary use -
// dozens of comments, suggestions and replies of everyday length - is taken whole. Without the
// bound, thirty-two suggestions of 900 KB were taken and one cold websocket load of their document
// took 296 MiB.
func TestSuggestionsCannotGrowADocumentsMarginPastWhatItMayHold(t *testing.T) {
	handler := newTestHandler(t)
	var words strings.Builder
	for index := range 100 {
		fmt.Fprintf(&words, "q%02dx ", index)
	}
	spec := words.String() + "\n"
	suggest := func(issue string, quote int, replacement string) *httptest.ResponseRecorder {
		return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
			"body":       "a suggestion",
			"anchor":     map[string]any{"artifact": "spec", "quote": fmt.Sprintf("q%02dx", quote)},
			"suggestion": map[string]string{"replace_with": replacement},
		}, "alice")
	}
	refused := func(response *httptest.ResponseRecorder, want string) {
		t.Helper()
		if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), want) {
			t.Fatalf("status=%d body=%.500s, want 413 CAP_EXCEEDED saying %q", response.Code, response.Body.String(), want)
		}
	}

	issue := createInteractionIssue(t, handler, "TEST", "Margin", spec)
	stored := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	refused(suggest(issue.Key, 0, strings.Repeat("a", 300_000)), "holds at most 256 KiB (262144 bytes) of text")
	for index := range 4 {
		if response := suggest(issue.Key, index+1, strings.Repeat("a", 230_000)); response.Code != http.StatusCreated {
			t.Fatalf("suggestion %d of four that fit the margin: status=%d body=%.300s", index+1, response.Code, response.Body.String())
		}
	}
	refused(suggest(issue.Key, 5, strings.Repeat("a", 230_000)), "a document's margin holds at most 1 MiB (1048576 bytes)")
	for _, read := range []string{"/text", "/blocks"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s after the refused suggestion: status=%d body=%.300s", read, response.Code, response.Body.String())
		}
	}
	if text := documentMarkdown(t, handler, issue.PrimaryArtifactID); text != stored {
		t.Fatalf("the document after the suggestions reads %q, want it as it was, %q", text, stored)
	}

	ordinary := createInteractionIssue(t, handler, "ORD", "Ordinary margin", spec)
	sentence := strings.Repeat("A sentence of ordinary length. ", 16)
	for index := range 60 {
		var response *httptest.ResponseRecorder
		if index%3 == 0 {
			response = suggest(ordinary.Key, index, sentence)
		} else {
			response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+ordinary.Key+"/comments", map[string]any{
				"body":   sentence,
				"anchor": map[string]any{"artifact": "spec", "quote": fmt.Sprintf("q%02dx", index)},
			}, "alice")
		}
		if response.Code != http.StatusCreated {
			t.Fatalf("ordinary comment %d: status=%d body=%.300s", index+1, response.Code, response.Body.String())
		}
		if index%5 == 0 {
			root := decodeBody[model.Comment](t, response)
			reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+ordinary.Key+"/comments", map[string]any{"body": sentence, "reply_to": root.ID}, "alice")
			if reply.Code != http.StatusCreated {
				t.Fatalf("a reply to comment %d: status=%d body=%.300s", index+1, reply.Code, reply.Body.String())
			}
		}
	}
}

// refusedTooLarge reports whether response is a write refused as too large to store: 413
// CAP_EXCEEDED whose message is the refusal as the document service words it, opening "document too
// large to store", with none of a route's or a service method's own prose before it.
func refusedTooLarge(t *testing.T, response *httptest.ResponseRecorder) bool {
	t.Helper()
	if response.Code != http.StatusRequestEntityTooLarge {
		return false
	}
	var body struct{ Code, Error string }
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the refusal %.300s: %v", response.Body.String(), err)
	}
	return body.Code == "CAP_EXCEEDED" && strings.HasPrefix(body.Error, "document too large to store: ")
}

// An answer is stored on its ask as well as in its block, and when a deleted block returns to the
// document settlement writes the stored answer back into it, so it is caller text in the document
// as the answer route's is: settlement writes it back where the document has room for it, and where
// the document would pass what one upload may hold and grow, it restores the block's state and
// withholds the answer's text, which the ask keeps. A 75-byte insert of an answered ask's block
// left a 225 KB document, and twenty-four answers of 900 KB, each weighed against a document their
// deleted blocks had left small, came back in one 1,852-byte edit as a 21.6 MB document.
func TestSettlementCannotGrowADocumentPastWhatOneUploadMayHoldByRestoringAnswers(t *testing.T) {
	handler, _ := blockAskHandler(t)
	prose := strings.Repeat("word ", 180_000)
	block := func(index int) string {
		return fmt.Sprintf(":::ask{#ask-%d urgency=\"med\" multiple=\"false\"}\nQuestion %d?\n:::\n", index, index)
	}
	issue := createInteractionIssue(t, handler, "TEST", "Restored answers", "Context\n\n"+block(0)+"\n"+block(1))
	ids := make([]string, 2)
	for index := range ids {
		blockID := fmt.Sprintf("ask-%d", index)
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, blockID, fmt.Sprintf("Question %d?", index))
		ids[index] = blockAskID(t, handler, issue.Key, blockID)
	}
	settled := 0
	edit := func(op map[string]string) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]string{op}}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("edit %v: status=%d body=%.300s", op, response.Code, response.Body.String())
		}
		settled++
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, fmt.Sprintf("settled-%d", settled))
	}
	answer := func(index int) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ids[index]+"/answer", map[string]any{"text": prose}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("answer ask-%d: status=%d body=%.300s", index, response.Code, response.Body.String())
		}
	}
	// directive is ask-0's directive line as the document holds it.
	directive := func() (string, int) {
		t.Helper()
		text := documentMarkdown(t, handler, issue.PrimaryArtifactID)
		for line := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(line, ":::ask{#ask-0 ") {
				return line, len(text)
			}
		}
		t.Fatalf("the document holds no ask-0 block:\n%.500s", text)
		return "", 0
	}

	answer(0)
	edit(map[string]string{"op": "delete", "block": "ask-0"})
	edit(map[string]string{"op": "insert", "after": "end", "markdown": block(0)})
	if line, size := directive(); !strings.Contains(line, `state="answered"`) || !strings.Contains(line, `answer="`+prose) {
		t.Fatalf("the returned block of an answer the %d-byte document has room for reads %.300s, want its state and its answer", size, line)
	}

	edit(map[string]string{"op": "delete", "block": "ask-0"})
	answer(1)
	edit(map[string]string{"op": "insert", "after": "end", "markdown": block(0)})
	line, size := directive()
	if size > 1<<20 {
		t.Fatalf("returning an answered ask's 75-byte block left a %d-byte document, past the 1 MiB one upload may hold", size)
	}
	if !strings.Contains(line, `state="answered"`) || !strings.Contains(line, `answered_by="alice"`) || strings.Contains(line, "answer=") || strings.Contains(line, "selected=") {
		t.Fatalf("the returned block of an answer the document has no room for reads %.300s, want its state and who answered, without the answer's text", line)
	}
	if ask := readBlockAsk(t, handler, ids[0]); ask.State != "answered" || ask.Answer == nil || ask.Answer.Text == nil || *ask.Answer.Text != prose {
		t.Fatalf("the ask whose answer settlement withheld from its block = %#v, want it answered with its text", ask)
	}
	for _, read := range []string{"/text", "/blocks"} {
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+read, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s after the withheld answer: status=%d body=%.300s", read, response.Code, response.Body.String())
		}
	}

	edit(map[string]string{"op": "delete", "block": "ask-1"})
	if line, size := directive(); !strings.Contains(line, `answer="`+prose) {
		t.Fatalf("once the %d-byte document has room again, ask-0's block reads %.300s, want its answer back", size, line)
	}
}

// Settlement weighs each answer it writes back into a returning block on its own, in document
// order: two answers of 400 KB returned in one edit to a 300 KB document, which has room for one,
// leave the first block with its answer and the second saying who answered and when without it,
// and the second's answer comes back once the document has room for it. Withheld together, neither
// came back until both fit.
func TestSettlementGivesBackEachReturnedAnswerTheDocumentHasRoomFor(t *testing.T) {
	handler, _ := blockAskHandler(t)
	prose := strings.Repeat("words ", 50_000)
	answerText := strings.Repeat("word ", 80_000)
	block := func(index int) string {
		return fmt.Sprintf(":::ask{#ask-%d urgency=\"med\" multiple=\"false\"}\nQuestion %d?\n:::\n", index, index)
	}
	issue := createInteractionIssue(t, handler, "TEST", "Returned answers", prose+"\n\n"+block(0)+"\n"+block(1))
	ids := make([]string, 2)
	for index := range ids {
		blockID := fmt.Sprintf("ask-%d", index)
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, blockID, fmt.Sprintf("Question %d?", index))
		ids[index] = blockAskID(t, handler, issue.Key, blockID)
	}
	settled := 0
	edit := func(op map[string]string) {
		t.Helper()
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{"ops": []map[string]string{op}}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("edit %v: status=%d body=%.300s", op, response.Code, response.Body.String())
		}
		settled++
		settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, fmt.Sprintf("settled-%d", settled))
	}
	for index := range ids {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ids[index]+"/answer", map[string]any{"text": answerText}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("answer ask-%d: status=%d body=%.300s", index, response.Code, response.Body.String())
		}
		edit(map[string]string{"op": "delete", "block": fmt.Sprintf("ask-%d", index)})
	}
	edit(map[string]string{"op": "insert", "after": "end", "markdown": block(0) + "\n" + block(1)})
	text := documentMarkdown(t, handler, issue.PrimaryArtifactID)
	if len(text) > 1<<20 {
		t.Fatalf("returning both blocks left a %d-byte document, past the 1 MiB one upload may hold", len(text))
	}
	if line := askDirective(t, text, "ask-0"); !strings.Contains(line, `answer="`+answerText) {
		t.Fatalf("the first returned block, whose answer the document has room for, reads %.300s, want its answer", line)
	}
	if line := askDirective(t, text, "ask-1"); !strings.Contains(line, `state="answered" answered_by="alice" answered_at="`) || strings.Contains(line, "answer=") {
		t.Fatalf("the second returned block, whose answer the document has no room for beside the first, reads %.300s, want who answered and when, without the answer", line)
	}
	edit(map[string]string{"op": "delete", "block": "ask-0"})
	if line := askDirective(t, documentMarkdown(t, handler, issue.PrimaryArtifactID), "ask-1"); !strings.Contains(line, `answer="`+answerText) {
		t.Fatalf("once the document has room, the second block reads %.300s, want its answer back", line)
	}
}

// An anchor mark carries the id of whoever anchored it, which no rendering carries and every load of
// the document builds, and an ask's anchor has no margin record to weigh it: thirty asks anchored
// by a session whose id is 200 KB were each taken, leaving a 210-byte rendering over six megabytes
// of live document. The marks on a document's text carry at most 1 MiB, so the anchor that would
// pass it is refused with 413 CAP_EXCEEDED saying so, and the document still reads.
func TestAnchorMarksCannotGrowALiveDocumentPastWhatItsMarksMayCarry(t *testing.T) {
	handler, _ := blockAskHandler(t)
	words := make([]string, 30)
	for index := range words {
		words[index] = fmt.Sprintf("word%02d", index)
	}
	issue := createInteractionIssue(t, handler, "TEST", "Anchors", strings.Join(words, " ")+"\n")
	actor := map[string]any{"kind": "session", "id": "s" + strings.Repeat("a", 200_000)}
	refused := 0
	for index, word := range words {
		response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
			"question": fmt.Sprintf("Why %s?", word),
			"anchor":   map[string]any{"artifact": "spec", "quote": word},
			"actor":    actor,
		})
		if response.Code == http.StatusCreated && refused == 0 {
			continue
		}
		if !refusedTooLarge(t, response) || !strings.Contains(response.Body.String(), "carry at most 1 MiB") {
			t.Fatalf("anchored ask %d: status=%d body=%.500s, want 201 until one is refused with 413 CAP_EXCEEDED naming the marks' bound, and 413 after it", index+1, response.Code, response.Body.String())
		}
		if refused == 0 {
			refused = index + 1
			t.Logf("anchored ask %d refused: %.300s", refused, response.Body.String())
		}
	}
	if refused == 0 || refused > 6 {
		t.Fatalf("the first refused anchor was %d of %d, want the sixth, whose 200 KB actor id passes 1 MiB", refused, len(words))
	}
	if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/text", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("the document's text after the refused anchors: status=%d body=%.300s", response.Code, response.Body.String())
	}
}
