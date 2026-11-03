package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// treesRequest is a GET as the dashboard makes it when trees is true (`Dispatch-Trees: 1`) and as
// every other client makes it when it is false.
func treesRequest(t *testing.T, handler http.Handler, target string, trees bool) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	if trees {
		request.Header.Set("Dispatch-Trees", "1")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s (trees=%v): status=%d body=%s", target, trees, response.Code, response.Body.String())
	}
	return response
}

// valueAt is the JSON value standing at a dotted path of the response body, and whether the body
// holds it at all. A number in the path indexes a list.
func valueAt(t *testing.T, response *httptest.ResponseRecorder, path string) (json.RawMessage, bool) {
	t.Helper()
	var value any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode the answer %s: %v", response.Body.String(), err)
	}
	for _, step := range strings.Split(path, ".") {
		switch held := value.(type) {
		case map[string]any:
			next, ok := held[step]
			if !ok {
				return nil, false
			}
			value = next
		case []any:
			index := 0
			for _, digit := range step {
				index = index*10 + int(digit-'0')
			}
			if index >= len(held) {
				return nil, false
			}
			value = held[index]
		default:
			return nil, false
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the value at %s: %v", path, err)
	}
	return encoded, true
}

// requireDisplayTree fails unless the answer holds no tree at treePath without the header, and
// the tree ReadForDisplay makes of the text standing at textPath with it. The text is read from
// the answer itself, so the check is against what Dispatch stored rather than what the test sent.
func requireDisplayTree(t *testing.T, handler http.Handler, target, textPath, treePath string) {
	t.Helper()
	bare := treesRequest(t, handler, target, false)
	if _, held := valueAt(t, bare, treePath); held {
		t.Errorf("GET %s without the header carries %s", target, treePath)
	}
	raw, held := valueAt(t, bare, textPath)
	if !held {
		t.Fatalf("GET %s carries no %s", target, textPath)
	}
	var markdown string
	if err := json.Unmarshal(raw, &markdown); err != nil {
		t.Fatalf("GET %s: %s is %s, not a text", target, textPath, raw)
	}
	got, held := valueAt(t, treesRequest(t, handler, target, true), treePath)
	if !held {
		t.Fatalf("GET %s with the header carries no %s", target, treePath)
	}
	want := sameShape(t, pmdoc.DisplayJSON(markdown, pmdoc.DisplayLatest))
	if !bytes.Equal(got, want) {
		t.Errorf("GET %s: %s is\n\t%s\nwant ReadForDisplay's tree of %q\n\t%s", target, treePath, got, markdown, want)
	}
}

// sameShape is tree's encoding with its keys in the order valueAt's own round trip leaves them,
// so two encodings of one tree compare as the one value they are.
func sameShape(t *testing.T, tree json.RawMessage) json.RawMessage {
	t.Helper()
	var value any
	if err := json.Unmarshal(tree, &value); err != nil {
		t.Fatalf("decode the tree %s: %v", tree, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode the tree: %v", err)
	}
	return encoded
}

// Every route class the dashboard reads stored text from carries that text's display tree beside
// it when, and only when, the request holds `Dispatch-Trees: 1`, and the tree is the one
// pmdoc.ReadForDisplay makes of exactly that text. The routes covered here are the issue's
// comment list and one comment's thread, the issue's ask list and one ask, the issue's message
// list, the issue's event log, a document's text, and the caller's own answers; the search
// snippet, whose tree is cut and marked rather than read whole, has its own test below.
func TestEveryReadCarriesItsDisplayTreesOnlyUnderTheHeader(t *testing.T) {
	handler := newTestHandler(t)
	const (
		commentBody  = "Please check **this** before the deploy."
		messageBody  = "The *staging* run is green."
		askQuestion  = "Ship the ~~draft~~ **release** today?"
		optionLabel  = "Yes, `ship it`"
		answerText   = "Shipping **now**."
		documentText = "# Spec\n\nThe **plan** is written here.\n"
	)
	issue := createInteractionIssue(t, handler, "TREE", "Display trees", documentText)

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
		map[string]any{"body": commentBody}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID

	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages",
		map[string]any{"body": messageBody}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}
	messageID := decodeBody[model.Message](t, message).ID

	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": askQuestion,
		"options":  []map[string]string{{"label": optionLabel}},
	}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	askID := decodeBody[model.Ask](t, ask).ID
	answered := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer",
		map[string]any{"selected": []string{optionLabel}, "text": answerText}, "alice")
	if answered.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answered.Code, answered.Body.String())
	}

	for _, test := range []struct{ name, target, textPath, treePath string }{
		{"the issue's comment list", "/api/v1/issues/" + issue.Key + "/comments", "0.body", "0.body_tree"},
		{"one comment's thread", "/api/v1/comments/" + commentID, "comment.body", "comment.body_tree"},
		{"the issue's message list", "/api/v1/issues/" + issue.Key + "/messages", "0.message.body", "0.message.body_tree"},
		{"one message's thread", "/api/v1/messages/" + messageID, "message.body", "message.body_tree"},
		{"the issue's ask list", "/api/v1/issues/" + issue.Key + "/asks", "0.question", "0.question_tree"},
		{"an ask option in the list", "/api/v1/issues/" + issue.Key + "/asks", "0.options.0.label", "0.options.0.label_tree"},
		{"one ask", "/api/v1/asks/" + askID, "ask.question", "ask.question_tree"},
		{"one ask's answer", "/api/v1/asks/" + askID, "ask.answer.text", "ask.answer.text_tree"},
		{"the caller's answer history", "/api/v1/me/answers", "rows.0.question", "rows.0.question_tree"},
		{"a document's text", "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/text", "markdown", "markdown_tree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireDisplayTree(t, handler, test.target, test.textPath, test.treePath)
		})
	}

	// The event log's trees ride in the envelope's own map, keyed by the payload field's path.
	t.Run("the issue's event log", func(t *testing.T) {
		target := "/api/v1/issues/" + issue.Key + "/events"
		bare := treesRequest(t, handler, target, false)
		if strings.Contains(bare.Body.String(), `"trees"`) {
			t.Errorf("GET %s without the header carries trees: %s", target, bare.Body.String())
		}
		var events []struct {
			Type  string                     `json:"type"`
			Trees map[string]json.RawMessage `json:"trees"`
		}
		if err := json.Unmarshal(treesRequest(t, handler, target, true).Body.Bytes(), &events); err != nil {
			t.Fatalf("decode the event log: %v", err)
		}
		want := map[string]json.RawMessage{
			"comment.created": pmdoc.DisplayJSON(commentBody, pmdoc.DisplayLatest),
			"message.created": pmdoc.DisplayJSON(messageBody, pmdoc.DisplayLatest),
		}
		seen := map[string]bool{}
		for _, event := range events {
			if tree, carried := want[event.Type]; carried {
				seen[event.Type] = true
				if !bytes.Equal(event.Trees["payload.body"], tree) {
					t.Errorf("%s trees[payload.body] = %s, want %s", event.Type, event.Trees["payload.body"], tree)
				}
			}
			if event.Type == "ask.opened" {
				if !bytes.Equal(event.Trees["payload.question"], pmdoc.DisplayJSON(askQuestion, pmdoc.DisplayLatest)) {
					t.Errorf("ask.opened trees[payload.question] = %s", event.Trees["payload.question"])
				}
				if !bytes.Equal(event.Trees["payload.options.0.label"], pmdoc.DisplayJSON(optionLabel, pmdoc.DisplayLatest)) {
					t.Errorf("ask.opened trees[payload.options.0.label] = %s", event.Trees["payload.options.0.label"])
				}
				seen["ask.opened"] = true
			}
		}
		for _, kind := range []string{"comment.created", "message.created", "ask.opened"} {
			if !seen[kind] {
				t.Errorf("the event log carried no %s", kind)
			}
		}
	})
}

// Every answer says its body depends on the trees header and on the encoding the caller takes, so
// no cache between a browser and Dispatch hands a reader with trees an answer written for a reader
// without them, or a caller that takes no gzip one that was compressed.
func TestEveryAnswerVariesOnTheTreesHeaderAndTheEncoding(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "VARY", "Vary", "A spec")
	for _, target := range []string{"/api/v1/issues/" + issue.Key, "/api/v1/issues/" + issue.Key + "/comments", "/api/v1/projects"} {
		response := treesRequest(t, handler, target, false)
		got := slices.Sorted(slices.Values(response.Header().Values("Vary")))
		if !slices.Equal(got, []string{"Accept-Encoding", "Dispatch-Trees"}) {
			t.Errorf("GET %s: Vary = %v, want Dispatch-Trees and Accept-Encoding", target, got)
		}
	}
}

// A search hit's snippet carries the tree of the headline itself, balanced as the cut left it and
// with the match marked, and carries it only under the header.
func TestASearchHitCarriesTheTreeOfItsSnippet(t *testing.T) {
	handler := newTestHandler(t)
	createInteractionIssue(t, handler, "SNIP", "Astrolabe", "The **astrolabe** measures altitude.")
	const target = "/api/v1/search?q=astrolabe"

	var bare model.SearchResponse
	if err := json.Unmarshal(treesRequest(t, handler, target, false).Body.Bytes(), &bare); err != nil {
		t.Fatalf("decode the search answer: %v", err)
	}
	if len(bare.Results) == 0 {
		t.Fatalf("the search found nothing: %s", treesRequest(t, handler, target, false).Body.String())
	}
	for _, result := range bare.Results {
		if result.SnippetTree != nil {
			t.Errorf("a hit carries snippet_tree without the header: %s", result.SnippetTree)
		}
	}

	var marked model.SearchResponse
	if err := json.Unmarshal(treesRequest(t, handler, target, true).Body.Bytes(), &marked); err != nil {
		t.Fatalf("decode the search answer: %v", err)
	}
	for _, result := range marked.Results {
		if result.SnippetTree == nil {
			t.Fatalf("hit %s carries no snippet_tree", result.ID)
		}
		highlighted := strings.Join(highlightedText(t, result.SnippetTree), " ")
		if !strings.Contains(strings.ToLower(highlighted), "astrolabe") {
			t.Errorf("hit %s highlights %q, want the matched word", result.ID, highlighted)
		}
		if strings.Contains(treeText(t, result.SnippetTree), markStart) {
			t.Errorf("hit %s keeps a sentinel in its tree: %s", result.ID, result.SnippetTree)
		}
	}
}

// snippetNode is a display tree as a test reads it: enough of a ProseMirror node to walk its text.
type snippetNode struct {
	Type    string        `json:"type"`
	Text    string        `json:"text"`
	Marks   []snippetMark `json:"marks"`
	Content []snippetNode `json:"content"`
}

type snippetMark struct {
	Type string `json:"type"`
}

// treeText is every character of text the tree holds, in document order.
func treeText(t *testing.T, tree json.RawMessage) string {
	t.Helper()
	var root snippetNode
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	var out strings.Builder
	var walk func(snippetNode)
	walk = func(node snippetNode) {
		out.WriteString(node.Text)
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)
	return out.String()
}

// highlightedText is the text of every node the snippet tree marks as what the query matched.
func highlightedText(t *testing.T, tree json.RawMessage) []string {
	t.Helper()
	var root snippetNode
	if err := json.Unmarshal(tree, &root); err != nil {
		t.Fatalf("decode the snippet tree %s: %v", tree, err)
	}
	var found []string
	var walk func(snippetNode)
	walk = func(node snippetNode) {
		for _, mark := range node.Marks {
			if mark.Type == pmdoc.DisplayHighlightMark {
				found = append(found, node.Text)
				break
			}
		}
		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)
	return found
}

// What the trees cost a reader, measured as a proxy sees it: one issue's comment list gzipped
// with and without the header. The numbers are logged rather than bounded - a tree's size is the
// text's - and the check is only that the trees are what grew the answer.
func TestTheTreesCostOfACommentList(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SIZE", "Comment list size", "A spec")
	for index := range 20 {
		body := "Comment " + strconv.Itoa(index) + ": the **deploy** is `green`, see [the run](https://example.com/run) " +
			"and the *staging* check.\n\n- one item\n- another item\n"
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			map[string]any{"body": body}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create comment %d: status=%d body=%s", index, response.Code, response.Body.String())
		}
	}
	target := "/api/v1/issues/" + issue.Key + "/comments"
	gzipped := func(trees bool) int {
		body := treesRequest(t, handler, target, trees).Body.Bytes()
		var squeezed bytes.Buffer
		writer := gzip.NewWriter(&squeezed)
		if _, err := writer.Write(body); err != nil {
			t.Fatalf("gzip the answer: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close the gzip writer: %v", err)
		}
		t.Logf("%s trees=%v: %d bytes, %d gzipped", target, trees, len(body), squeezed.Len())
		return squeezed.Len()
	}
	without, with := gzipped(false), gzipped(true)
	if with <= without {
		t.Errorf("the answer with trees gzips to %d bytes, not over the %d without them", with, without)
	}
}
