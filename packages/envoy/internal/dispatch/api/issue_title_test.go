package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// An issue title is at most contracts.IssueTitleMax UTF-16 units, the dashboard's maxLength, on
// creation and on a retitle alike, counted after the trim both routes apply. A longer one is
// refused by name before anything is written, so a creation over the cap creates nothing, and a
// retitle over it leaves the title it had.
func TestIssueTitlesAreCappedOnCreateAndRetitle(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "CAP", "name": "Cap"}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	type issue struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	}
	create := func(title string) issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CAP", "title": title, "force": true}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("create a title of %d units: status=%d body=%.300s", len16(title), response.Code, response.Body.String())
		}
		return decodeBody[issue](t, response)
	}
	read := func(key string) issue {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("read %s: status=%d body=%.300s", key, response.Code, response.Body.String())
		}
		return decodeBody[issue](t, response)
	}
	count := func() int {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=CAP", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("list CAP: status=%d body=%.300s", response.Code, response.Body.String())
		}
		return len(decodeBody[[]issue](t, response))
	}

	// 1,000 two-byte characters, padded with spaces the trim removes: at the limit in UTF-16
	// units, twice it in bytes.
	atLimit := strings.Repeat("é", contracts.IssueTitleMax)
	created := create("  " + atLimit + "  ")
	if created.Title != atLimit {
		t.Fatalf("created title is %d units, want the %d it was sent without its spaces", len16(created.Title), contracts.IssueTitleMax)
	}
	retitledTo := strings.Repeat("a", contracts.IssueTitleMax)
	retitled := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"title": " " + retitledTo + " "}, "alice")
	if retitled.Code != http.StatusOK {
		t.Fatalf("retitle at the limit: status=%d body=%.300s", retitled.Code, retitled.Body.String())
	}
	if title := read(created.Key).Title; title != retitledTo {
		t.Fatalf("retitled title is %d units, want %d", len16(title), contracts.IssueTitleMax)
	}
	issues := count()

	for name, test := range map[string]struct {
		title  string
		length int
	}{
		"one character over": {strings.Repeat("a", contracts.IssueTitleMax+1), contracts.IssueTitleMax + 1},
		// 501 characters outside the Basic Multilingual Plane are 1,002 UTF-16 units.
		"counted in UTF-16 units": {strings.Repeat("😀", contracts.IssueTitleMax/2+1), contracts.IssueTitleMax + 2},
	} {
		t.Run(name, func(t *testing.T) {
			want := fmt.Sprintf("title is %d characters over the %d-character limit (%d/%d)",
				test.length-contracts.IssueTitleMax, contracts.IssueTitleMax, test.length, contracts.IssueTitleMax)
			for route, send := range map[string]func() *httptest.ResponseRecorder{
				"create": func() *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "CAP", "title": test.title, "force": true}, "alice")
				},
				"retitle": func() *httptest.ResponseRecorder {
					return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+created.Key, map[string]any{"title": test.title}, "alice")
				},
			} {
				response := send()
				body := response.Body.String()
				refusal := decodeBody[struct {
					Code  string `json:"code"`
					Error string `json:"error"`
				}](t, response)
				if response.Code != http.StatusBadRequest || refusal.Code != "CAP_EXCEEDED" || refusal.Error != want {
					t.Fatalf("%s: status=%d body=%.300s, want 400 CAP_EXCEEDED %q", route, response.Code, body, want)
				}
			}
			if after := count(); after != issues {
				t.Fatalf("the refused creation left %d issues in CAP, want %d", after, issues)
			}
			if title := read(created.Key).Title; title != retitledTo {
				t.Fatalf("the refused retitle left a title of %d units, want the %d it had", len16(title), contracts.IssueTitleMax)
			}
		})
	}
}
