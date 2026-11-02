package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// fileCommentResponse is a created comment as the file-anchor tests read it.
type fileCommentResponse struct {
	ID     string        `json:"id"`
	Anchor *model.Anchor `json:"anchor"`
}

// uploadFile uploads content as name to an issue or a project (owner is its route prefix) as alice.
func uploadFile(t *testing.T, handler http.Handler, owner, name, contentType string, content []byte) uploadedArtifact {
	t.Helper()
	response := multipartRequest(t, handler, owner+"/artifacts", map[string]string{"name": name}, name, contentType, content, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("upload %s: status=%d body=%s", name, response.Code, response.Body.String())
	}
	return decodeBody[uploadedArtifact](t, response)
}

func postFileComment(t *testing.T, handler http.Handler, target string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodPost, target, body, "alice")
}

func requireRefused(t *testing.T, response *httptest.ResponseRecorder, code, wording string) {
	t.Helper()
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) ||
		!strings.Contains(response.Body.String(), wording) {
		t.Fatalf("status=%d body=%s, want 400 %s saying %q", response.Code, response.Body.String(), code, wording)
	}
}

// A person draws a box on an issue's picture and comments on it: the comment's anchor names the
// picture, the version it was written on, and the box as the fractions it was sent with, and the
// stored anchor reads back the same through the comment list and the comment's own read.
func TestACommentOnPartOfAPictureKeepsItsBoxAndVersion(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Chart review", "The chart.")
	picture := uploadImage(t, handler, issue.Key, "chart.png", "first chart")

	created := postFileComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "This bar looks low.",
		"anchor": map[string]any{
			"artifact": "chart-png",
			"box":      map[string]any{"left": 0.62, "top": 0.1, "width": 0.3, "height": 0.25},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("comment on part of the picture: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[fileCommentResponse](t, created)
	want := model.Anchor{
		Kind: model.AnchorKindFile, ArtifactID: picture.Artifact.ID, MarkID: comment.ID, Version: 1,
		Box: &model.AnchorBox{Left: 0.62, Top: 0.1, Width: 0.3, Height: 0.25},
	}
	requireFileAnchor(t, "the created comment", comment.Anchor, want)

	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments?artifact="+picture.Artifact.ID, nil, "alice")
	if listed.Code != http.StatusOK {
		t.Fatalf("list the picture's comments: status=%d body=%s", listed.Code, listed.Body.String())
	}
	comments := decodeBody[[]fileCommentResponse](t, listed)
	if len(comments) != 1 || comments[0].ID != comment.ID {
		t.Fatalf("the picture's comments = %#v, want the one comment", comments)
	}
	requireFileAnchor(t, "the listed comment", comments[0].Anchor, want)
	requireFileAnchor(t, "the comment's read", readThreadComment(t, handler, comment.ID).Anchor, want)
	if raw := created.Body.String(); strings.Contains(raw, `"anchor_block"`) {
		t.Fatalf("a file anchor has no block, but the comment carries one: %s", raw)
	}
}

func requireFileAnchor(t *testing.T, what string, got *model.Anchor, want model.Anchor) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s has no anchor, want %#v", what, want)
	}
	gotBox, wantBox := got.Box, want.Box
	got.Box, want.Box = nil, nil
	defer func() { got.Box = gotBox }()
	if *got != want || (gotBox == nil) != (wantBox == nil) || (gotBox != nil && *gotBox != *wantBox) {
		t.Fatalf("%s anchor = %#v box %#v, want %#v box %#v", what, *got, gotBox, want, wantBox)
	}
}

// A comment on a whole file names the file and its latest version, or the version the request
// names; a file that is not a picture takes no box, and a version the file does not have is refused.
func TestACommentOnAWholeFileRecordsItsVersion(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Results", "The results.")
	owner := "/api/v1/issues/" + issue.Key
	uploadFile(t, handler, owner, "results.csv", "text/csv", []byte("run,score\n1,0.5\n"))
	table := uploadFile(t, handler, owner, "results.csv", "text/csv", []byte("run,score\n1,0.5\n2,0.7\n"))
	if table.Artifact.Kind != "file" || table.Version.Number != 2 {
		t.Fatalf("second upload of the table = %#v version %d, want a file at version 2", table.Artifact, table.Version.Number)
	}

	latest := postFileComment(t, handler, owner+"/comments", map[string]any{
		"body": "Run 2 is new.", "anchor": map[string]any{"artifact": table.Artifact.ID},
	})
	if latest.Code != http.StatusCreated {
		t.Fatalf("comment on the whole file: status=%d body=%s", latest.Code, latest.Body.String())
	}
	comment := decodeBody[fileCommentResponse](t, latest)
	requireFileAnchor(t, "the whole-file comment", comment.Anchor, model.Anchor{
		Kind: model.AnchorKindFile, ArtifactID: table.Artifact.ID, MarkID: comment.ID, Version: 2,
	})

	first := postFileComment(t, handler, owner+"/comments", map[string]any{
		"body": "Run 1 alone.", "anchor": map[string]any{"artifact": "results-csv", "version": 1},
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("comment on the first version: status=%d body=%s", first.Code, first.Body.String())
	}
	if anchor := decodeBody[fileCommentResponse](t, first).Anchor; anchor == nil || anchor.Version != 1 {
		t.Fatalf("the first version's comment anchor = %#v, want version 1", anchor)
	}

	requireRefused(t, postFileComment(t, handler, owner+"/comments", map[string]any{
		"body": "Where?", "anchor": map[string]any{"artifact": "results-csv", "version": 3},
	}), "INVALID_ANCHOR", "version 3 is not a version of results.csv")
	requireRefused(t, postFileComment(t, handler, owner+"/comments", map[string]any{
		"body": "Where?", "anchor": map[string]any{"artifact": "results-csv", "version": 0},
	}), "INVALID_ANCHOR", "version 0 is not a version of results.csv")
	requireRefused(t, postFileComment(t, handler, owner+"/comments", map[string]any{
		"body": "This cell.",
		"anchor": map[string]any{
			"artifact": "results-csv",
			"box":      map[string]any{"left": 0, "top": 0, "width": 0.5, "height": 0.5},
		},
	}), "NOT_PICTURE", "a box marks part of a picture")
}

// Each rule a box breaks is refused with the rule's own words, and nothing is written.
func TestABoxOutsideThePictureIsRefused(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Boxes", "The chart.")
	uploadImage(t, handler, issue.Key, "chart.png", "chart")
	for _, test := range []struct {
		name string
		box  map[string]any
		says string
	}{
		{"left of the picture", map[string]any{"left": -0.1, "top": 0, "width": 0.5, "height": 0.5}, "left and top must be at least 0"},
		{"above the picture", map[string]any{"left": 0, "top": -0.01, "width": 0.5, "height": 0.5}, "left and top must be at least 0"},
		{"no width", map[string]any{"left": 0.2, "top": 0.2, "width": 0, "height": 0.5}, "width and height must be greater than 0"},
		{"no height", map[string]any{"left": 0.2, "top": 0.2, "width": 0.5, "height": -0.5}, "width and height must be greater than 0"},
		{"past the right edge", map[string]any{"left": 0.8, "top": 0, "width": 0.3, "height": 0.5}, "left + width must be at most 1"},
		{"past the bottom", map[string]any{"left": 0, "top": 0.6, "width": 0.3, "height": 0.5}, "top + height must be at most 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireRefused(t, postFileComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
				"body": "Here.", "anchor": map[string]any{"artifact": "chart-png", "box": test.box},
			}), "INVALID_ANCHOR", test.says)
		})
	}
	t.Run("a box missing a side", func(t *testing.T) {
		response := postFileComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Here.", "anchor": map[string]any{"artifact": "chart-png", "box": map[string]any{"left": 0, "top": 0, "width": 0.5}},
		})
		requireRefused(t, response, "INVALID_ANCHOR", "box needs left, top, width and height")
	})
	t.Run("a box that is not numbers", func(t *testing.T) {
		response := postFileComment(t, handler, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
			"body": "Here.", "anchor": map[string]any{"artifact": "chart-png", "box": map[string]any{"left": "a", "top": 0, "width": 0.5, "height": 0.5}},
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("a box of text: status=%d body=%s, want 400", response.Code, response.Body.String())
		}
	})
	listed := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/comments", nil, "alice")
	if comments := decodeBody[[]fileCommentResponse](t, listed); len(comments) != 0 {
		t.Fatalf("refused boxes wrote comments: %#v", comments)
	}
}

// The file anchor is for an uploaded file that is not a Markdown document, and for a comment: a
// document's comment names a passage, an ask names a passage, a suggestion replaces quoted text, and
// a version or box never rides beside a quote or a mark.
func TestAFileAnchorIsOnlyForACommentOnAFile(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Anchors", "The quick brown fox")
	uploadImage(t, handler, issue.Key, "chart.png", "chart")
	comments := "/api/v1/issues/" + issue.Key + "/comments"

	requireRefused(t, postFileComment(t, handler, comments, map[string]any{
		"body": "All of it.", "anchor": map[string]any{"artifact": "spec"},
	}), "INVALID_ANCHOR", "a comment on a document names a passage")
	requireRefused(t, postFileComment(t, handler, comments, map[string]any{
		"body": "That box.", "anchor": map[string]any{"artifact": "spec", "box": map[string]any{"left": 0, "top": 0, "width": 0.5, "height": 0.5}},
	}), "INVALID_ANCHOR", "a comment on a document names a passage")
	requireRefused(t, postFileComment(t, handler, comments, map[string]any{
		"body": "Brown.", "anchor": map[string]any{"artifact": "spec", "quote": "brown", "version": 1},
	}), "INVALID_ANCHOR", "version and box go only with a comment on an uploaded file")
	requireRefused(t, postFileComment(t, handler, comments, map[string]any{
		"body": "Brighter.", "suggestion": map[string]any{"replace_with": "a brighter chart"},
		"anchor": map[string]any{"artifact": "chart-png"},
	}), "INVALID_ANCHOR", "a suggestion replaces quoted text")
	requireRefused(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Is this bar right?", "anchor": map[string]any{"artifact": "chart-png"},
	}, "alice"), "INVALID_ANCHOR", "an ask names a passage of a document")
}

// A thread on a file is a thread like any other: a reply joins it, and resolving, reopening and
// editing its root all succeed, though no document holds a mark for it.
func TestAThreadOnAFileTakesRepliesResolveReopenAndEdit(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	issue := createInteractionIssue(t, handler, "TEST", "Threads", "The chart.")
	uploadImage(t, handler, issue.Key, "chart.png", "chart")
	comments := "/api/v1/issues/" + issue.Key + "/comments"
	created := postFileComment(t, handler, comments, map[string]any{
		"body": "This bar.", "anchor": map[string]any{
			"artifact": "chart-png", "box": map[string]any{"left": 0.1, "top": 0.1, "width": 0.2, "height": 0.2},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("comment on part of the picture: status=%d body=%s", created.Code, created.Body.String())
	}
	root := decodeBody[fileCommentResponse](t, created)

	reply := postFileComment(t, handler, comments, map[string]any{"body": "Fixed in v2.", "reply_to": root.ID})
	if reply.Code != http.StatusCreated {
		t.Fatalf("reply in the picture's thread: status=%d body=%s", reply.Code, reply.Body.String())
	}
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/comments/" + root.ID + "/resolve", nil},
		{http.MethodPost, "/api/v1/comments/" + root.ID + "/reopen", nil},
		{http.MethodPatch, "/api/v1/comments/" + root.ID, map[string]any{"body": "This bar, the third."}},
	} {
		response := dispatchRequest(t, handler, step.method, step.path, step.body, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s: status=%d body=%s", step.method, step.path, response.Code, response.Body.String())
		}
	}
	read := readThreadComment(t, handler, root.ID)
	if read.Body != "This bar, the third." || read.Resolved || read.Anchor == nil || read.Anchor.Box == nil {
		t.Fatalf("the thread's root after edit, resolve and reopen = %#v", read)
	}
	resolveReply := postFileComment(t, handler, comments, map[string]any{"body": "Thanks.", "reply_to": root.ID})
	if resolveReply.Code != http.StatusCreated {
		t.Fatalf("second reply: status=%d body=%s", resolveReply.Code, resolveReply.Body.String())
	}
}

// A project's picture takes the same comment through the project artifact's own route, and the
// comment.created event carries the box, which is what an agent the comment reaches is shown.
func TestACommentOnAProjectPictureCarriesItsBoxOnItsEvent(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{files: filestest.NewMemory()})
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	picture := uploadFile(t, handler, "/api/v1/projects/CORE", "diagram.png", "image/png", pngBytes("diagram"))
	created := postFileComment(t, handler, "/api/v1/artifacts/"+picture.Artifact.ID+"/comments", map[string]any{
		"body": "This arrow points the wrong way.",
		"anchor": map[string]any{
			"artifact": picture.Artifact.ID, "box": map[string]any{"left": 0.5, "top": 0.5, "width": 0.5, "height": 0.5},
		},
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("comment on the project picture: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[fileCommentResponse](t, created)
	events := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+picture.Artifact.ID+"/events", nil, "alice")
	if events.Code != http.StatusOK {
		t.Fatalf("list the picture's events: status=%d body=%s", events.Code, events.Body.String())
	}
	var listed []struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(events.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	for _, event := range listed {
		if event.Type != "comment.created" {
			continue
		}
		payload := decodeJSONBytes[fileCommentResponse](t, event.Payload)
		requireFileAnchor(t, "the comment.created payload", payload.Anchor, model.Anchor{
			Kind: model.AnchorKindFile, ArtifactID: picture.Artifact.ID, MarkID: comment.ID, Version: 1,
			Box: &model.AnchorBox{Left: 0.5, Top: 0.5, Width: 0.5, Height: 0.5},
		})
		return
	}
	t.Fatalf("no comment.created among the picture's events: %s", events.Body.String())
}

func decodeJSONBytes[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}
