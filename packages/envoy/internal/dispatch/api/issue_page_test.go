package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func createListedIssues(t *testing.T, handler http.Handler, project string, count int) []string {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": project,
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
	}
	keys := make([]string, count)
	for index := range keys {
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": project, "title": project + " listed issue " + strconv.Itoa(index), "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create issue %d: status=%d body=%s", index, response.Code, response.Body.String())
		}
		keys[index] = decodeBody[model.Issue](t, response).Key
	}
	return keys
}

func summaryKeys(issues []model.IssueSummary) []string {
	keys := make([]string, len(issues))
	for index, issue := range issues {
		keys[index] = issue.Key
	}
	return keys
}

// listedKeys reads the unpaged listing, which is a bare array.
func listedKeys(t *testing.T, handler http.Handler, query string) []string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list ?%s: status=%d body=%s", query, response.Code, response.Body.String())
	}
	return summaryKeys(decodeBody[[]model.IssueSummary](t, response))
}

// readPage reads one page, reporting (not stopping on) an answer that is not one: a route that
// ignored the paging parameters answers the whole listing as a bare array.
func readPage(t *testing.T, handler http.Handler, query string) (model.IssueSummaryPage, bool) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?"+query, nil, "alice")
	if response.Code != http.StatusOK {
		t.Errorf("?%s: status=%d body=%s, want 200 with a page", query, response.Code, response.Body.String())
		return model.IssueSummaryPage{}, false
	}
	var page model.IssueSummaryPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		var every []model.IssueSummary
		if json.Unmarshal(response.Body.Bytes(), &every) == nil {
			t.Errorf("?%s: status=200 with all %d issues as a bare list, want a page", query, len(every))
		} else {
			t.Errorf("?%s: decode page: %v; body=%s", query, err, response.Body.String())
		}
		return model.IssueSummaryPage{}, false
	}
	return page, true
}

// A caller that asks the listing for a page gets that page and how many issues the filters
// matched, rather than every issue (LEGION-406).
func TestListIssuesServesTheRequestedPage(t *testing.T) {
	handler := newTestHandler(t)
	keys := createListedIssues(t, handler, "TEST", 7)
	createListedIssues(t, handler, "OTHER", 2)
	for _, test := range []struct {
		query         string
		want          []string
		limit, offset int
	}{
		{"project=TEST&limit=3", keys[0:3], 3, 0},
		{"project=TEST&limit=3&offset=3", keys[3:6], 3, 3},
		{"project=TEST&limit=3&offset=6", keys[6:7], 3, 6},
		{"project=TEST&offset=5", keys[5:7], 50, 5},
		{"project=TEST&limit=250", keys, 250, 0},
		{"project=TEST&limit=3&offset=7", []string{}, 3, 7},
		{"project=TEST&limit=3&offset=900", []string{}, 3, 900},
	} {
		page, ok := readPage(t, handler, test.query)
		if !ok {
			continue
		}
		if got := summaryKeys(page.Issues); !slices.Equal(got, test.want) || page.Total != len(keys) ||
			page.Limit != test.limit || page.Offset != test.offset {
			t.Errorf("?%s: page %v total %d limit %d offset %d, want %v total %d limit %d offset %d",
				test.query, got, page.Total, page.Limit, page.Offset, test.want, len(keys), test.limit, test.offset)
		}
	}
	for _, key := range keys[1:3] {
		if response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/issues/"+key+"/state", map[string]bool{"pinned": true}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("pin %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	if page, ok := readPage(t, handler, "pinned=true&limit=1&offset=1"); ok &&
		(!slices.Equal(summaryKeys(page.Issues), keys[2:3]) || page.Total != 2) {
		t.Errorf("pinned page = %v total %d, want [%s] total 2", summaryKeys(page.Issues), page.Total, keys[2])
	}
}

// Consecutive pages of a filtered listing hold every row it matches exactly once, whatever the
// page size, including a page size that divides it and one past it. The listing is narrowed by a
// label in SQL and by route_status after the query, so the page must be cut after both. The
// rows share one rank and creation time, so only the key orders them.
func TestListIssuePagesCoverAFilteredListingOnce(t *testing.T) {
	roster := &fakeRoster{}
	handler, database := routeStatusServer(t, roster)
	keys := createListedIssues(t, handler, "TEST", 10)
	for index, key := range keys {
		patch := map[string]any{}
		if index%3 != 2 {
			patch["labels"] = []string{"frontend"}
		}
		if index%2 == 0 {
			patch["route"] = "role:reviewer"
		}
		if len(patch) == 0 {
			continue
		}
		if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, patch, "alice"); response.Code != http.StatusOK {
			t.Fatalf("patch %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
	}
	roster.set("reviewer-session", "reviewer")
	if _, err := database.Pool.Exec(context.Background(),
		"update issues set rank = 'U', created_at = '2026-09-30T12:00:00Z' where project_key = 'TEST'"); err != nil {
		t.Fatalf("tie every issue's rank and creation time: %v", err)
	}

	for _, filter := range []string{"project=TEST&label=frontend", "project=TEST&route_status=live", "project=TEST"} {
		whole := listedKeys(t, handler, filter)
		if len(whole) < 5 {
			t.Fatalf("?%s lists %v, want a listing long enough to page", filter, whole)
		}
		for _, limit := range []int{1, 2, 3, len(whole), len(whole) + 1} {
			walked := []string{}
			for offset := 0; ; offset += limit {
				query := filter + "&limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset)
				page, ok := readPage(t, handler, query)
				if !ok {
					break
				}
				if page.Total != len(whole) {
					t.Errorf("?%s: total %d, want %d", query, page.Total, len(whole))
				}
				walked = append(walked, summaryKeys(page.Issues)...)
				if len(page.Issues) < limit {
					break
				}
			}
			if !slices.Equal(walked, whole) {
				t.Errorf("?%s pages of %d walk %v, want each of %v once, in order", filter, limit, walked, whole)
			}
		}
	}
}

// Tied issues come in key order when the listing also holds issues that do not tie with them.
// Postgres's sort does not keep the order its input arrives in, so without the key it hands back
// a tie among other rows in an order of its own making, and a write anywhere in the listing can
// reshuffle it between two pages of one walk.
func TestListIssuesOrdersTiesByKeyAmongOtherIssues(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	tied := []string{}
	for _, project := range []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO", "FOXTROT", "GOLF", "HOTEL", "INDIA", "JULIET", "KILO", "LIMA"} {
		// A project's first issue is rank U, the rest rank after it.
		tied = append(tied, createListedIssues(t, handler, project, 4)[0])
	}
	for index := len(tied) - 1; index >= 0; index-- {
		if _, err := database.Pool.Exec(context.Background(),
			"update issues set created_at = '2026-09-30T12:00:00Z' where key = $1", tied[index]); err != nil {
			t.Fatalf("tie %s: %v", tied[index], err)
		}
	}
	listed := listedKeys(t, handler, "")
	got := slices.DeleteFunc(slices.Clone(listed), func(key string) bool { return !slices.Contains(tied, key) })
	want := slices.Clone(tied)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("the tied first issues list as %v among %d issues, want key order %v", got, len(listed), want)
	}
}

// Without limit or offset the listing is what it was: every matching issue, as a bare array.
func TestListIssuesWithoutPagingAnswersEveryIssue(t *testing.T) {
	handler := newTestHandler(t)
	test := createListedIssues(t, handler, "TEST", 3)
	other := createListedIssues(t, handler, "OTHER", 1)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Body.String(), "[") {
		t.Fatalf("?project=TEST: status=%d body=%.120s, want a bare array", response.Code, response.Body.String())
	}
	if got := summaryKeys(decodeBody[[]model.IssueSummary](t, response)); !slices.Equal(got, test) {
		t.Fatalf("?project=TEST lists %v, want every TEST issue %v", got, test)
	}
	if got := listedKeys(t, handler, ""); len(got) != len(test)+len(other) {
		t.Fatalf("unfiltered listing = %v, want all %d issues", got, len(test)+len(other))
	}
}

// The seven filters narrow the listing as before, paged or not.
func TestListIssuesFiltersStillApplyBesidePaging(t *testing.T) {
	roster := &fakeRoster{}
	handler, database := routeStatusServer(t, roster)
	test := createListedIssues(t, handler, "TEST", 4)
	other := createListedIssues(t, handler, "OTHER", 1)
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
			t.Errorf("?%s lists %v, want %v", filter.query, got, filter.want)
		}
		if page, ok := readPage(t, handler, filter.query+"&limit=1"); ok &&
			(!slices.Equal(summaryKeys(page.Issues), filter.want) || page.Total != len(filter.want)) {
			t.Errorf("?%s&limit=1 pages %v total %d, want %v total %d", filter.query, summaryKeys(page.Issues), page.Total, filter.want, len(filter.want))
		}
	}
}

// A page the listing cannot serve is refused naming the parameter, and cursor, which the listing
// does not page with, is refused rather than answered with the first page.
func TestListIssuesRefusesAPageItCannotServe(t *testing.T) {
	handler := newTestHandler(t)
	createListedIssues(t, handler, "TEST", 2)
	for _, test := range []struct{ query, parameter string }{
		{"limit=0", "limit"},
		{"limit=251", "limit"},
		{"limit=-1", "limit"},
		{"limit=x", "limit"},
		{"limit=1.5", "limit"},
		{"limit=", "limit"},
		{"limit=1&limit=2", "limit"},
		{"offset=-1", "offset"},
		{"offset=x", "offset"},
		{"offset=", "offset"},
		{"limit=1&offset=1&offset=2", "offset"},
		{"cursor=abc", "cursor"},
		{"limit=5&cursor=abc", "cursor"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST&"+test.query, nil, "alice")
		if response.Code != http.StatusBadRequest {
			t.Errorf("?%s: status=%d body=%.120s, want 400 INVALID_QUERY naming %s", test.query, response.Code, response.Body.String(), test.parameter)
			continue
		}
		body := decodeBody[struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}](t, response)
		if body.Code != "INVALID_QUERY" || !strings.HasPrefix(body.Error, test.parameter+" ") {
			t.Errorf("?%s: refusal %+v, want INVALID_QUERY naming %s", test.query, body, test.parameter)
		}
	}
}
