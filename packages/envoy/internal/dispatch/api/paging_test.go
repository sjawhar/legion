package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// pagingRefusal is the body of a refused paging parameter.
type pagingRefusal struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func createPagingIssues(t *testing.T, handler http.Handler, project string, count int) []string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": project,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	keys := make([]string, count)
	for index := range keys {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": project + " paging issue " + string(rune('A'+index)), "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %d: status=%d body=%s", index, response.Code, response.Body.String())
		}
		keys[index] = decodeBody[model.Issue](t, response).Key
	}
	return keys
}

func listedKeys(t *testing.T, handler http.Handler, query string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list ?%s: status=%d body=%s", query, response.Code, response.Body.String())
	}
	keys := []string{}
	for _, issue := range decodeBody[[]model.IssueSummary](t, response) {
		keys = append(keys, issue.Key)
	}
	return keys
}

// A caller that asks the issue listing for a page is refused, not handed every issue: a 200 with
// the whole list reads exactly like a page, so the caller would believe it had paged (LEGION-406).
func TestListIssuesRefusesPagingParameters(t *testing.T) {
	handler := newTestHandler(t)
	createPagingIssues(t, handler, "TEST", 3)
	for _, test := range []struct{ query, parameter string }{
		{"project=TEST&limit=1", "limit"},
		{"project=TEST&offset=1", "offset"},
		{"project=TEST&cursor=abc", "cursor"},
		{"project=TEST&limit=999", "limit"},
		{"limit=", "limit"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+test.query, nil, "alice")
		if response.Code != http.StatusBadRequest {
			if response.Code == http.StatusOK {
				t.Errorf("?%s: status=200 with all %d issues, want 400", test.query, len(decodeBody[[]model.IssueSummary](t, response)))
			} else {
				t.Errorf("?%s: status=%d body=%s, want 400", test.query, response.Code, response.Body.String())
			}
			continue
		}
		refusal := decodeBody[pagingRefusal](t, response)
		if refusal.Code != "INVALID_QUERY" ||
			!strings.HasPrefix(refusal.Error, test.parameter+" is not a query parameter of GET /api/v1/issues") ||
			!strings.Contains(refusal.Error, "unpaginated") ||
			!strings.Contains(refusal.Error, "dispatch_issues tool pages it with its limit and offset") {
			t.Errorf("?%s: refusal %+v does not name %s, the unpaginated listing and the tool that pages it", test.query, refusal, test.parameter)
		}
	}
}

// Without a paging parameter the listing is what it was: every matching issue, in rank order.
func TestListIssuesWithoutPagingAnswersEveryIssue(t *testing.T) {
	handler := newTestHandler(t)
	test := createPagingIssues(t, handler, "TEST", 3)
	other := createPagingIssues(t, handler, "OTHER", 1)
	if got := listedKeys(t, handler, "project=TEST"); !slices.Equal(got, test) {
		t.Fatalf("?project=TEST lists %v, want every TEST issue %v", got, test)
	}
	if got := listedKeys(t, handler, ""); len(got) != len(test)+len(other) {
		t.Fatalf("unfiltered listing = %v, want all %d issues", got, len(test)+len(other))
	}
}

// The refusal reads only the paging parameters: each of the listing's seven filters still narrows
// the list.
func TestListIssuesFiltersStillApplyBesideThePagingRefusal(t *testing.T) {
	roster := &fakeRoster{}
	handler, database := routeStatusServer(t, roster)
	test := createPagingIssues(t, handler, "TEST", 4)
	other := createPagingIssues(t, handler, "OTHER", 1)
	parent, child, labelled, routed := test[0], test[1], test[2], test[3]
	for key, patch := range map[string]map[string]any{
		parent:   {"status": "todo", "priority": 0},
		child:    {"parent": parent, "priority": 2},
		labelled: {"labels": []string{"frontend"}},
		routed:   {"route": "role:reviewer"},
	} {
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, patch, "alice"); response.Code != http.StatusOK {
			t.Fatalf("patch %s %v: status=%d body=%s", key, patch, response.Code, response.Body.String())
		}
	}
	roster.set("reviewer-session", "reviewer")
	watermark := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	for _, key := range test {
		updated := watermark.Add(-time.Hour)
		if key == labelled {
			updated = watermark
		}
		if _, err := database.Pool.Exec(context.Background(), "update issues set updated_at = $2 where key = $1", key, updated); err != nil {
			t.Fatalf("set %s updated_at: %v", key, err)
		}
	}

	for _, filter := range []struct {
		query string
		want  []string
	}{
		{"project=OTHER", other},
		{"project=TEST&status=todo", []string{parent}},
		{"project=TEST&parent=" + parent, []string{child}},
		{"project=TEST&label=frontend", []string{labelled}},
		{"project=TEST&priority=2", []string{child}},
		{"project=TEST&updated_since=" + url.QueryEscape(watermark.Format(time.RFC3339)), []string{labelled}},
		{"project=TEST&route_status=live", []string{routed}},
	} {
		if got := listedKeys(t, handler, filter.query); !slices.Equal(got, filter.want) {
			t.Fatalf("?%s lists %v, want %v", filter.query, got, filter.want)
		}
	}
}

// No GET route answers a paging parameter it does not read: each refuses it naming the parameter
// and the route. The event logs and search read limit, and nothing reads offset or cursor.
func TestGetRoutesRefusePagingParametersTheyDoNotServe(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	served := map[string][]string{
		"/api/v1/issues/{key}/events":   {"limit"},
		"/api/v1/artifacts/{id}/events": {"limit"},
		"/api/v1/search":                {"limit"},
	}
	wildcard := regexp.MustCompile(`\{[^}]+\}`)
	checked := 0
	for _, route := range directServer(deps).routes() {
		if route.Method != http.MethodGet {
			continue
		}
		for _, parameter := range []string{"limit", "offset", "cursor"} {
			if slices.Contains(served[route.Pattern], parameter) {
				continue
			}
			target := wildcard.ReplaceAllString(route.Pattern, "x") + "?" + parameter + "=5"
			request := httptest.NewRequest(http.MethodGet, target, nil)
			// A route that ignored the parameter would serve the request, and the event streams
			// would hold it open: bound it, so the failure is a status rather than a hang.
			ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
			request = request.WithContext(ctx)
			request.Header.Set("X-Dispatch-User", "alice")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			cancel()
			if response.Code != http.StatusBadRequest {
				t.Errorf("GET %s: status=%d body=%.200s, want 400", target, response.Code, response.Body.String())
				continue
			}
			refusal := decodeBody[pagingRefusal](t, response)
			if refusal.Code != "INVALID_QUERY" || !strings.HasPrefix(refusal.Error, parameter+" is not a query parameter of GET "+route.Pattern) {
				t.Errorf("GET %s: refusal %+v does not name %s and the route", target, refusal, parameter)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no GET route was checked")
	}
}

// The routes that page keep serving what they read: an event log's limit and search's limit.
func TestPagedRoutesStillServeTheirOwnLimit(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Astrolabe", "An astrolabe measures altitude.")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "todo"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("move %s to todo: status=%d body=%s", issue.Key, response.Code, response.Body.String())
	}
	document := createProjectDocument(t, handler, "SRCH", "Sextant notes", "# Sextant notes\n")
	createProjectDocument(t, handler, "SRCH", "Sextant notes", "# Sextant notes, revised\n")
	for _, log := range []string{"/api/v1/issues/" + issue.Key + "/events", "/api/v1/artifacts/" + document.ID + "/events"} {
		read := func(target string) []model.Event {
			t.Helper()
			response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
			}
			return decodeBody[[]model.Event](t, response)
		}
		if all := read(log); len(all) < 2 {
			t.Fatalf("GET %s: %d events, want at least two for limit to cut", log, len(all))
		}
		if limited := read(log + "?limit=1"); len(limited) != 1 {
			t.Fatalf("GET %s?limit=1: %d events, want the one limit asks for", log, len(limited))
		}
	}
	if all := searchResponse(t, handler, "q=astrolabe"); len(all.Results) < 2 {
		t.Fatalf("search: %d results, want at least two for limit to cut", len(all.Results))
	}
	if limited := searchResponse(t, handler, "q=astrolabe&limit=1"); len(limited.Results) != 1 {
		t.Fatalf("search limit=1: %d results, want 1", len(limited.Results))
	}
}
