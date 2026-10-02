package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Saving a document or an issue title takes time linear in a pathological run, up to the route's
// own cap (LEGION-465). Before the fix Postgres's search indexing read a run of `a_` or `q_` in
// quadratic time, and pmdoc parsed `a_b*` in nearly quadratic time. On one loaded machine, before
// the fix and after it at the same moment, 64 KiB of `a_` saved in 46 s and 0.3 s, 128 KiB of
// `a_b*` in 33 s and 1.3-2.1 s, an issue titled with 32 KiB of `q_` in 12 s and 0.06-0.17 s, and
// the duplicate check against a 16 KiB title took 18-21 s and 0.12-0.31 s. Times after the fix
// follow the runner's load, so no wall-clock bound holds on every runner; each save is timed at
// growing sizes instead, and its growth is bounded (growsLinearly).
//
// A ladder's first size is small enough that the request's own cost is most of its time, about
// the same before the fix and after (1 KiB of `a_`: 39-48 ms before, 51-61 ms after); its second
// is where the code before the fix is many times slower, so that code fails the first step by
// several times the bound. The duplicate check's own cost is about 2 ms, which the title's
// passes at a few hundred bytes, so its ladder starts at 64 B. Each ladder ends at the route's
// cap: a document's 1 MiB, and 256 KiB for `)_`, whose 1 MiB is more items than one document
// update can hold (TestUploadRefusesADocumentTooLargeToStore); a title has no cap of its own, and
// 512 KiB stands for the request's 1 MiB. `)_` saved in linear time before the fix too and is held
// to the same growth.
//
// Comments and messages cap their text at 2,000 characters and an ask's question at 800, which
// bounds their cost: a comment of 2,000 characters of `a_` took 51 ms before the fix and 56 ms
// after, so no route under such a cap is timed.
//
// Settlement is held off. A document's settlement holds its issue's row while it renders, so a
// save timed while one ran would count the wait for it; and the test server's shutdown, which
// would otherwise spend its drain budget settling 1 MiB documents, finds no document loaded and
// leaves each to resume.
func TestSavingAPathologicalBodyIsBounded(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	documents := 0
	for _, test := range []struct {
		shape string
		sizes []int
	}{
		{"a_", []int{1 << 10, 64 << 10, 1 << 20}},
		{"a_b*", []int{2 << 10, 128 << 10, 1 << 20}},
		{")_", []int{256, 16 << 10, 256 << 10}},
	} {
		t.Run("document "+test.shape, func(t *testing.T) {
			growsLinearly(t, "saving a document of `"+test.shape+"`", test.sizes, func(size int) time.Duration {
				documents++
				body := strings.Repeat(test.shape, size/len(test.shape))
				started := time.Now()
				response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
					"name": fmt.Sprintf("pathological-%d.md", documents),
				}, "body.md", "text/markdown", []byte(body), "alice")
				elapsed := time.Since(started)
				if response.Code != http.StatusCreated {
					t.Fatalf("upload %s of %q: status=%d body=%.200s", sizeText(size), test.shape, response.Code, response.Body.String())
				}
				return elapsed
			})
		})
	}
	// Creating an issue reads titles through the parser three ways: the duplicate check's
	// to_tsvector over the new title and every title in the project, its ts_headline over each
	// near-duplicate's title, and the issues trigger. Every title gets a project of its own, so
	// no save reads the titles the ones before it stored; the duplicate check reads one stored
	// title, whose word `q` makes it the near-duplicate its headline is drawn from.
	projects := 0
	project := func(t *testing.T) string {
		t.Helper()
		projects++
		key := fmt.Sprintf("T%d", projects)
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": key, "name": key}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%.200s", key, response.Code, response.Body.String())
		}
		return key
	}
	titled := func(t *testing.T, key string, size int) time.Duration {
		t.Helper()
		started := time.Now()
		created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": key, "title": strings.Repeat("q_", size/2)}, "alice")
		elapsed := time.Since(started)
		if created.Code != http.StatusCreated {
			t.Fatalf("issue titled with %s of `q_`: status=%d body=%.200s", sizeText(size), created.Code, created.Body.String())
		}
		return elapsed
	}
	t.Run("issue title", func(t *testing.T) {
		growsLinearly(t, "creating an issue titled with `q_`", []int{512, 32 << 10, 512 << 10}, func(size int) time.Duration {
			return titled(t, project(t), size)
		})
	})
	t.Run("duplicate check", func(t *testing.T) {
		growsLinearly(t, "the duplicate check against a title of `q_`", []int{64, 16 << 10, 512 << 10}, func(size int) time.Duration {
			key := project(t)
			titled(t, key, size)
			started := time.Now()
			duplicate := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": key, "title": "q"}, "alice")
			elapsed := time.Since(started)
			if duplicate.Code != http.StatusConflict || !strings.Contains(duplicate.Body.String(), `"code":"POSSIBLE_DUPLICATE"`) {
				t.Fatalf("issue titled with a word of a %s title run: status=%d body=%.200s, want 409 POSSIBLE_DUPLICATE", sizeText(size), duplicate.Code, duplicate.Body.String())
			}
			return elapsed
		})
	})
}

// growthAllowance is how many times faster than its input a save's time may grow from one size to
// the next. Linear time grows at most as fast as the input, less while the request's own cost
// counts, and quadratic time as the input's square. Four times the input's growth lets a runner's
// load quadruple between two saves, and still refuses a quadratic save at any step over four;
// every step here is eight or more.
const growthAllowance = 4

// growsLinearly times what at each size in turn and fails t at the first size whose time grew
// more than growthAllowance times faster than the size did. The first size, where the request's
// own cost is most of the time, is timed three times before the second size and three times after
// it, and the fastest of the six kept: the first save's warm-up does not count as that cost, and a
// load spike inflates it only if it lasts the whole of the second size's save. A step over the
// bound times the larger size again and keeps the faster time, so a load spike during one save is
// not read as growth; after the first step it also times the smaller size again and keeps the
// slower, so load that rose between two saves is not either.
func growsLinearly(t *testing.T, what string, sizes []int, timed func(size int) time.Duration) {
	t.Helper()
	first := func() time.Duration { return min(timed(sizes[0]), timed(sizes[0]), timed(sizes[0])) }
	before := first()
	for i := 1; i < len(sizes); i++ {
		smaller, size := sizes[i-1], sizes[i]
		growth := size / smaller
		took := timed(size)
		if i == 1 {
			before = min(before, first())
		}
		if took > time.Duration(growthAllowance*growth)*before {
			t.Logf("%s: %s at %s against %s at %s is over the bound; timing it again", what, took.Round(time.Millisecond), sizeText(size), before.Round(time.Millisecond), sizeText(smaller))
			took = min(took, timed(size))
			if i > 1 {
				before = max(before, timed(smaller))
			}
		}
		ratio := float64(took) / float64(before)
		if took > time.Duration(growthAllowance*growth)*before {
			t.Fatalf("%s took %s at %s, %.0f times the %s it took at %s: the input grew %d times, and a save linear in it may grow %d times at most (growthAllowance)",
				what, took.Round(time.Millisecond), sizeText(size), ratio, before.Round(time.Millisecond), sizeText(smaller), growth, growthAllowance*growth)
		}
		t.Logf("%s: %s at %s, %s at %s, %.1f times for an input %d times larger (at most %d)", what, before.Round(time.Millisecond), sizeText(smaller), took.Round(time.Millisecond), sizeText(size), ratio, growth, growthAllowance*growth)
		before = took
	}
}

func sizeText(size int) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", size>>20)
	case size >= 1<<10:
		return fmt.Sprintf("%d KiB", size>>10)
	}
	return fmt.Sprintf("%d B", size)
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
