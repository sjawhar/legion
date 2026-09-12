package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func searchRequest(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?"+query, nil, "alice")
}

type searchCorpus struct {
	issueKey          string
	primaryArtifactID string
	commentID         string
	askID             string
	messageID         string
}

func seedSearchCorpus(t *testing.T, handler http.Handler) searchCorpus {
	t.Helper()
	issue := createInteractionIssue(t, handler, "SRCH", "Navigation instruments", "# Instruments\n\nThe astrolabe measures altitude.\n\n"+strings.Repeat("Filler sentence. ", 400)+"\n")

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Replace the sextant diagram.",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID

	ask := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Keep the quadrant?",
		"options":  []map[string]string{{"label": "Yes"}},
	}, "alice")
	if ask.Code != http.StatusCreated {
		t.Fatalf("create ask: status=%d body=%s", ask.Code, ask.Body.String())
	}
	askID := decodeBody[model.Ask](t, ask).ID
	answer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+askID+"/answer", map[string]string{
		"text": "Use an alidade.",
	}, "alice")
	if answer.Code != http.StatusOK {
		t.Fatalf("answer ask: status=%d body=%s", answer.Code, answer.Body.String())
	}

	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Compass calibration done.",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}
	messageID := decodeBody[model.Message](t, message).ID

	return searchCorpus{
		issueKey:          issue.Key,
		primaryArtifactID: issue.PrimaryArtifactID,
		commentID:         commentID,
		askID:             askID,
		messageID:         messageID,
	}
}

func searchResponse(t *testing.T, handler http.Handler, query string) model.SearchResponse {
	t.Helper()
	response := searchRequest(t, handler, query)
	if response.Code != http.StatusOK {
		t.Fatalf("search %q: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return decodeBody[model.SearchResponse](t, response)
}

func TestSearchFindsEveryKindWithSnippetsAndHrefs(t *testing.T) {
	handler := newTestHandler(t)
	corpus := seedSearchCorpus(t, handler)

	cases := []struct {
		query string
		kind  string
		id    string
		href  string
		mark  string
	}{
		{"astrolabe", "document", corpus.primaryArtifactID, "/issues/" + corpus.issueKey + "/spec?q=astrolabe", "<mark>astrolabe</mark>"},
		{"sextant", "comment", corpus.commentID, "/issues/" + corpus.issueKey + "/comments/" + corpus.commentID, "<mark>sextant</mark>"},
		{"quadrant", "ask", corpus.askID, "/issues/" + corpus.issueKey + "/asks/" + corpus.askID, "<mark>quadrant</mark>"},
		{"alidade", "ask", corpus.askID, "/issues/" + corpus.issueKey + "/asks/" + corpus.askID, "<mark>alidade</mark>"},
		{"compass", "message", corpus.messageID, "/issues/" + corpus.issueKey + "/log", "<mark>Compass</mark>"},
		{"instruments", "issue", corpus.issueKey, "/issues/" + corpus.issueKey, "<mark>instruments</mark>"},
	}

	for _, test := range cases {
		t.Run(test.query, func(t *testing.T) {
			body := searchResponse(t, handler, "q="+test.query)
			if len(body.Results) == 0 {
				t.Fatal("search returned no results")
			}
			result := body.Results[0]
			if result.Kind != test.kind || result.ID != test.id || result.Href != test.href {
				t.Fatalf("result = %#v, want kind=%q id=%q href=%q", result, test.kind, test.id, test.href)
			}
			if result.Issue.Key != corpus.issueKey || result.Issue.Status != "triage" {
				t.Fatalf("result issue = %#v, want key=%q status=triage", result.Issue, corpus.issueKey)
			}
			if result.Owner != (model.SearchOwner{Kind: "issue", Key: corpus.issueKey}) {
				t.Fatalf("result owner = %#v, want issue %q", result.Owner, corpus.issueKey)
			}
			if !strings.Contains(result.Snippet, test.mark) {
				t.Fatalf("snippet %q does not contain %q", result.Snippet, test.mark)
			}
			if body.TookMS < 0 {
				t.Fatalf("took_ms = %d, want non-negative", body.TookMS)
			}
		})
	}
}

func TestSearchDocumentSnippetWindowsAroundADeepMatch(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Navigation", strings.Repeat("Filler ", 2_000)+"The astrolabe measures altitude.")

	body := searchResponse(t, handler, "q=astrolabe")
	if len(body.Results) == 0 || body.Results[0].ID != issue.PrimaryArtifactID {
		t.Fatalf("results = %#v, want document %q first", body.Results, issue.PrimaryArtifactID)
	}
	if !strings.Contains(body.Results[0].Snippet, "<mark>astrolabe</mark>") {
		t.Fatalf("deep search snippet %q does not contain marked match", body.Results[0].Snippet)
	}
}

func TestSearchReadsOnlyTheLatestDocumentVersion(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Instruments", "The astrolabe measures altitude.\n")

	edit := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":   []map[string]string{{"op": "replace", "find": "astrolabe", "with": "sextant"}},
		"actor": sessionActor(),
	})
	if edit.Code != http.StatusOK {
		t.Fatalf("edit document: status=%d body=%s", edit.Code, edit.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var versions int
		if err := database.Pool.QueryRow(context.Background(), "select count(*) from artifact_versions where artifact_id = $1", issue.PrimaryArtifactID).Scan(&versions); err != nil {
			t.Fatalf("count artifact versions: %v", err)
		}
		if versions >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiting for second artifact version")
		}
		time.Sleep(10 * time.Millisecond)
	}

	old := searchResponse(t, handler, "q=astrolabe")
	for _, result := range old.Results {
		if result.Kind == "document" && result.ID == issue.PrimaryArtifactID {
			t.Fatalf("stale document result = %#v", result)
		}
	}
	latest := searchResponse(t, handler, "q=sextant")
	if len(latest.Results) == 0 || latest.Results[0].Kind != "document" || latest.Results[0].ID != issue.PrimaryArtifactID {
		t.Fatalf("results = %#v, want latest document %q", latest.Results, issue.PrimaryArtifactID)
	}
}

func TestSearchRanksTitleHitsAboveBodyHits(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "SRCH", "Astrolabe", "Nothing here.")
	second := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "SRCH", "title": "Other", "spec": "An astrolabe appears twice: astrolabe.",
	}, "alice")
	if second.Code != http.StatusCreated {
		t.Fatalf("create body-hit issue: status=%d body=%s", second.Code, second.Body.String())
	}

	body := searchResponse(t, handler, "q=astrolabe")
	if len(body.Results) == 0 || body.Results[0].Kind != "issue" || body.Results[0].ID != first.Key {
		t.Fatalf("results = %#v, want title hit for %q first", body.Results, first.Key)
	}
}

func TestSearchFiltersByProjectAndHonoursLimit(t *testing.T) {
	handler := newTestHandler(t)
	first := createInteractionIssue(t, handler, "SRCH", "Astrolabe", "An astrolabe measures altitude.")
	second := createInteractionIssue(t, handler, "OTHER", "Astrolabe", "An astrolabe measures altitude.")

	filtered := searchResponse(t, handler, "q=astrolabe&project=SRCH")
	if len(filtered.Results) == 0 {
		t.Fatal("project-filtered search returned no results")
	}
	for _, result := range filtered.Results {
		if result.Issue.Key != first.Key {
			t.Fatalf("project-filtered result = %#v, want only %q", result, first.Key)
		}
	}

	limited := searchResponse(t, handler, "q=astrolabe&limit=1")
	if len(limited.Results) != 1 {
		t.Fatalf("limited results = %#v, want exactly one", limited.Results)
	}
	if limited.Results[0].Issue.Key != first.Key && limited.Results[0].Issue.Key != second.Key {
		t.Fatalf("limited result = %#v, want one seeded issue", limited.Results[0])
	}

	empty := searchResponse(t, handler, "q=astrolabe&project=NONE")
	if empty.Results == nil || len(empty.Results) != 0 {
		t.Fatalf("unknown-project results = %#v, want non-nil empty slice", empty.Results)
	}
}

func TestSearchFindsStandaloneProjectDocumentsAndTheirDiscussions(t *testing.T) {
	handler := newTestHandler(t)
	for _, project := range []map[string]string{
		{"key": "CORE", "name": "Core"},
		{"key": "OTHER", "name": "Other"},
	} {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", project, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project["key"], response.Code, response.Body.String())
		}
	}

	document := createProjectDocument(t, handler, "CORE", "Navigation design", "# Design\nThe astrolabe finds latitude.\n")
	_ = createProjectDocument(t, handler, "OTHER", "Other design", "# Design\nThe astrolabe finds latitude.\n")

	askResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/asks", map[string]any{
		"question": "Should the quadrant be included?",
		"actor":    sessionActor(),
	})
	if askResponse.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", askResponse.Code, askResponse.Body.String())
	}
	ask := decodeBody[model.Ask](t, askResponse)

	commentResponse := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", map[string]any{
		"body":  "Keep the alidade in the diagram.",
		"actor": sessionActor(),
	})
	if commentResponse.Code != http.StatusCreated {
		t.Fatalf("create document comment: status=%d body=%s", commentResponse.Code, commentResponse.Body.String())
	}
	comment := decodeBody[model.Comment](t, commentResponse)

	owner := model.SearchOwner{
		Kind:       "document",
		Project:    "CORE",
		Slug:       document.Slug,
		ArtifactID: document.ID,
		Name:       document.Name,
	}
	cases := []struct {
		query string
		kind  string
		id    string
		href  string
	}{
		{"astrolabe", "document", document.ID, "/projects/CORE/documents/" + document.Slug + "?q=astrolabe"},
		{"quadrant", "ask", ask.ID, "/projects/CORE/documents/" + document.Slug + "?ask=" + ask.ID},
		{"alidade", "comment", comment.ID, "/projects/CORE/documents/" + document.Slug + "?comment=" + comment.ID},
	}
	for _, test := range cases {
		t.Run(test.query, func(t *testing.T) {
			results := searchResponse(t, handler, "q="+test.query)
			var result *model.SearchResult
			for index := range results.Results {
				candidate := &results.Results[index]
				if candidate.Kind == test.kind && candidate.ID == test.id {
					result = candidate
					break
				}
			}
			if result == nil {
				t.Fatalf("results = %#v, want kind=%q id=%q", results.Results, test.kind, test.id)
			}
			if result.Href != test.href {
				t.Fatalf("href = %q, want %q", result.Href, test.href)
			}
			if result.Owner != owner {
				t.Fatalf("owner = %#v, want %#v", result.Owner, owner)
			}
		})
	}

	filtered := searchResponse(t, handler, "q=astrolabe&project=CORE")
	if len(filtered.Results) != 1 || filtered.Results[0].Owner != owner {
		t.Fatalf("project-filtered results = %#v, want only %#v", filtered.Results, owner)
	}
}

func TestSearchOnlyReturnsServerInsertedMarks(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Navigation", "No search term here.")
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]string{
		"body": "<script>alert(1)</script> astrolabe <mark>x</mark>",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID

	body := searchResponse(t, handler, "q=astrolabe")
	if len(body.Results) == 0 || body.Results[0].Kind != "comment" || body.Results[0].ID != commentID {
		t.Fatalf("results = %#v, want comment %q first", body.Results, commentID)
	}
	snippet := body.Results[0].Snippet
	if strings.Contains(snippet, "<script>") || strings.Contains(snippet, "<mark>x</mark>") {
		t.Fatalf("snippet %q contains user markup", snippet)
	}
	if strings.Count(snippet, "<mark>") != 1 || !strings.Contains(snippet, "<mark>astrolabe</mark>") {
		t.Fatalf("snippet %q did not retain exactly one search mark", snippet)
	}
}

func TestMarkSnippetEscapesMarkupBeforeInjectingSearchMarks(t *testing.T) {
	snippet := markSnippet("<script>alert(1)</script> " + markStart + "astrolabe" + markEnd + " <mark>x</mark>")
	if !strings.Contains(snippet, "&lt;script&gt;") || !strings.Contains(snippet, "&lt;mark&gt;x&lt;/mark&gt;") {
		t.Fatalf("snippet %q did not escape user markup", snippet)
	}
	if strings.Count(snippet, "<mark>") != 1 || !strings.Contains(snippet, "<mark>astrolabe</mark>") {
		t.Fatalf("snippet %q did not retain exactly one search mark", snippet)
	}
}

func TestSearchRejectsInvalidQueries(t *testing.T) {
	handler := newTestHandler(t)
	cases := map[string]string{
		"q=a":                  "INVALID_QUERY",
		"":                     "INVALID_QUERY",
		"q=the":                "INVALID_QUERY",
		"q=astrolabe&limit=0":  "INVALID_LIMIT",
		"q=astrolabe&limit=51": "INVALID_LIMIT",
		"q=astrolabe&limit=x":  "INVALID_LIMIT",
	}
	for query, code := range cases {
		t.Run(query, func(t *testing.T) {
			response := searchRequest(t, handler, query)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("search %q: status=%d body=%s", query, response.Code, response.Body.String())
			}
			body := decodeBody[struct {
				Code string `json:"code"`
			}](t, response)
			if body.Code != code {
				t.Fatalf("search %q code=%q, want %q", query, body.Code, code)
			}
		})
	}
}

func TestSearchAuthentication(t *testing.T) {
	handler := newTestHandler(t)

	unauthenticated := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?q=astrolabe", nil, "")
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated search: status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}

	agent := agentRequest(t, handler, http.MethodGet, "/api/v1/search?q=astrolabe", nil, "agent-token")
	if agent.Code != http.StatusOK {
		t.Fatalf("agent search: status=%d body=%s", agent.Code, agent.Body.String())
	}

	wrongBearerRequest := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=astrolabe", nil)
	wrongBearerRequest.Header.Set("Authorization", "Bearer wrong-token")
	wrongBearer := httptest.NewRecorder()
	handler.ServeHTTP(wrongBearer, wrongBearerRequest)
	if wrongBearer.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer search: status=%d body=%s", wrongBearer.Code, wrongBearer.Body.String())
	}
}

func TestFirstTermSkipsOperators(t *testing.T) {
	cases := map[string]string{
		"astrolabe":         "astrolabe",
		"-daemon astrolabe": "astrolabe",
		`"merge queue" -x`:  "merge",
		"OR astrolabe":      "astrolabe",
		"-only":             "",
	}
	for query, want := range cases {
		t.Run(query, func(t *testing.T) {
			if got := firstTerm(query); got != want {
				t.Fatalf("firstTerm(%q) = %q, want %q", query, got, want)
			}
		})
	}
}
