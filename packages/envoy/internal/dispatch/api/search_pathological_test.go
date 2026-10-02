package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Saving a document, comment, ask or message whose text is a pathological run finishes in bounded
// time at the route's own cap (LEGION-465). Before the fix a 100 KB `a_` spec took 85 s through
// this route and a 1 MiB one hours, all of it in Postgres's search indexing; a 900 KB `)_` spec
// took 14 s in the Go document pipeline. The bound is ten times the slowest save measured after
// the fix on a development machine (a 1 MiB `a_b*` document: 2.3 s).
func TestSavingAPathologicalBodyIsBounded(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	// `)_` is linear with a large constant - every `)_` is an italic span, so 1 MiB of it is
	// 524,288 text nodes and more items than one document update can hold
	// (TestUploadRefusesADocumentTooLargeToStore) - and pmdoc's linear-time test holds it at
	// 1 MiB; here 256 KB keeps its settlement inside the test server's shutdown budget.
	for _, test := range []struct {
		shape string
		bytes int
	}{{"a_", 1 << 20}, {"a_b*", 1 << 20}, {")_", 256 << 10}} {
		shape := test.shape
		body := strings.Repeat(shape, test.bytes/len(shape))
		started := time.Now()
		response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
			"name": "pathological-" + strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' {
					return r
				}
				return 'x'
			}, shape) + ".md",
		}, "body.md", "text/markdown", []byte(body), "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("upload %d bytes of %q: status=%d body=%.200s", test.bytes, shape, response.Code, response.Body.String())
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("saving %d bytes of %q took %s, want under 30 s", test.bytes, shape, elapsed)
		}
	}
	// The 2,000-character routes, at their cap: the parser is quadratic in the run, so a run of
	// 2,000 `_`-joined characters cost 55 ms alone and a comment, ask and message each hold one.
	twoThousand := strings.Repeat("a_", 1000)
	for _, route := range []struct{ path, field string }{
		{"/api/v1/issues/" + issue.Key + "/comments", "body"},
		{"/api/v1/issues/" + issue.Key + "/messages", "body"},
	} {
		started := time.Now()
		response := dispatchRequest(t, handler, http.MethodPost, route.path, map[string]any{route.field: twoThousand}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("%s with a 2,000-character run: status=%d body=%.200s", route.path, response.Code, response.Body.String())
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("%s with a 2,000-character run took %s, want under 5 s", route.path, elapsed)
		}
	}
	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{"question": strings.Repeat("a_", 400)}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("ask with an 800-character run: status=%d body=%.200s", response.Code, response.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("ask with an 800-character run took %s, want under 5 s", elapsed)
	}
	// An issue's title has no cap of its own (the request's 1 MiB bounds it), and creating an
	// issue reads titles through the parser three ways: the duplicate check's to_tsvector over the
	// new title and every title in the project, its ts_headline over each near-duplicate's title,
	// and the issues trigger. The two runs below share no word, so both are created, the second
	// reading the first's stored title; the third title is a word of the first, so the first is
	// its near-duplicate and its headline is drawn from that 512 KiB run.
	for _, title := range []string{strings.Repeat("q_", 256<<10), strings.Repeat("r_", 256<<10)} {
		started := time.Now()
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "TEST", "title": title}, "alice")
		if created.Code != http.StatusCreated {
			t.Fatalf("issue with a 512 KiB title run: status=%d body=%.200s", created.Code, created.Body.String())
		}
		if elapsed := time.Since(started); elapsed > 30*time.Second {
			t.Errorf("creating an issue with a 512 KiB title run took %s, want under 30 s", elapsed)
		}
	}
	started = time.Now()
	duplicate := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "TEST", "title": "q"}, "alice")
	if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), `"code":"POSSIBLE_DUPLICATE"`) {
		t.Fatalf("issue titled with a word of a 512 KiB title run: status=%d body=%.200s, want 409 POSSIBLE_DUPLICATE", duplicate.Code, duplicate.Body.String())
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("the duplicate check against a 512 KiB title run took %s, want under 30 s", elapsed)
	}
}

// Ordinary text searches as it did before search_text: a title, a document, a comment, an ask
// with its answer and a message are each found by a word they hold, with the same snippet marks,
// and a word joined to others by underscores is still found by itself.
func TestSearchFindsOrdinaryAndUnderscoredTextAfterSearchText(t *testing.T) {
	handler := newTestHandler(t)
	corpus := seedSearchCorpus(t, handler)
	for _, test := range []struct{ query, kind, id string }{
		{"astrolabe", "document", corpus.primaryArtifactID},
		{"sextant", "comment", corpus.commentID},
		{"alidade", "ask", corpus.askID},
		{"compass calibration", "message", corpus.messageID},
		{"navigation instruments", "issue", corpus.issueKey},
	} {
		results := searchResponse(t, handler, "q="+url.QueryEscape(test.query)).Results
		found := false
		for _, result := range results {
			if result.Kind == test.kind && result.ID == test.id {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q: no %s %s among %d results", test.query, test.kind, test.id, len(results))
		}
	}
	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+corpus.issueKey+"/messages", map[string]any{
		"body": "set LEGION_E2E_MODEL_GATEWAY_URL and the pod_safety_overlay_name_for_each_role_in_the_bundle_of_prompts_the_daemon_snapshots_at_boot_time first",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create underscored message: status=%d body=%s", message.Code, message.Body.String())
	}
	for _, query := range []string{"legion_e2e_model_gateway_url", "gateway", "snapshots", "boot"} {
		results := searchResponse(t, handler, "q="+url.QueryEscape(query)).Results
		found := false
		for _, result := range results {
			if result.Kind == "message" && strings.Contains(result.Snippet, "<mark>") {
				found = true
			}
		}
		if !found {
			t.Errorf("search %q: the underscored message was not found with a marked snippet", query)
		}
	}
}

// A document whose formatting cannot be stored - 1 MiB of `)_` is 524,288 italic spans, over the
// 1,048,576 items one document update may hold - is refused as the cap it hit, with the count, on
// a new document (SeedText) and on a replacement (ReplaceText), never a 500.
func TestUploadRefusesADocumentTooLargeToStore(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	body := strings.Repeat(")_", (1<<20)/2)
	// A new document (SeedText): refused, and nothing is created.
	response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "too-large.md"}, "body.md", "text/markdown", []byte(body), "alice")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), "1048576") {
		t.Fatalf("seed of a document too large to store: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the 1048576-item cap", response.Code, response.Body.String())
	}
	if listing := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts", nil, "alice"); strings.Contains(listing.Body.String(), "too-large") {
		t.Fatalf("the refused seed left an artifact behind: %.300s", listing.Body.String())
	}
	// A replacement of a stored document (ReplaceText): refused the same way, and the document keeps
	// its version.
	if first := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "body.md", "text/markdown", []byte("# Notes\n"), "alice"); first.Code != http.StatusCreated {
		t.Fatalf("seed notes.md: status=%d body=%.300s", first.Code, first.Body.String())
	}
	response = multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "body.md", "text/markdown", []byte(body), "alice")
	if response.Code != http.StatusRequestEntityTooLarge || !strings.Contains(response.Body.String(), `"code":"CAP_EXCEEDED"`) || !strings.Contains(response.Body.String(), "1048576") {
		t.Fatalf("replacement by a document too large to store: status=%d body=%.400s, want 413 CAP_EXCEEDED naming the 1048576-item cap", response.Code, response.Body.String())
	}
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/notes-md/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"# Notes\n"`) {
		t.Fatalf("notes.md after the refused replacement: status=%d body=%.300s, want its first version", text.Code, text.Body.String())
	}
}
