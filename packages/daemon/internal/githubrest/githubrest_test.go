package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// Post sends its body as JSON with the installation token, reads a 2xx answer such as GitHub's
// 201 Created, and gives any other answer back as an *Answer naming the method, the path and the
// status, so a caller can tell GitHub's refusal from a failed request.
func TestPostSendsJSONAndAnswersWithTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Ref string `json:"ref"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(r.Body).Decode(&sent) != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		if sent.Ref == "refs/heads/taken" {
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ref":"` + sent.Ref + `"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var created struct {
		Ref string `json:"ref"`
	}
	if err := client.Post(context.Background(), "/git/refs", map[string]string{"ref": "refs/heads/new"}, &created); err != nil || created.Ref != "refs/heads/new" {
		t.Fatalf("Post = %+v, %v; want the 201 answer read", created, err)
	}
	err := client.Post(context.Background(), "/git/refs?x=1", map[string]string{"ref": "refs/heads/taken"}, nil)
	var answer *Answer
	if !errors.As(err, &answer) || answer.Method != http.MethodPost || answer.Path != "/git/refs" || answer.Status != http.StatusUnprocessableEntity {
		t.Fatalf("Post of a taken ref = %v, want an *Answer for POST /git/refs with 422", err)
	}
}

// GetPages follows GitHub's next pages wherever GitHub names them on the API's own origin, its
// /repositories/<id>/ form included, and an answer on such a page names that page's path. A next
// page on another host is refused before the token is sent there.
func TestGetPagesFollowsNextPagesOnTheAPIsOriginOnly(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Write([]byte(`["leaked"]`))
	}))
	defer other.Close()
	var nextHost atomic.Pointer[string]
	var pageTwo atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			next := *nextHost.Load() + "/repositories/42/rules/branches/main?per_page=100&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next", <`+next+`>; rel="last"`)
			w.Write([]byte(`["first"]`))
		case "/repositories/42/rules/branches/main":
			if status := int(pageTwo.Load()); status != http.StatusOK {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, status)
				return
			}
			w.Write([]byte(`["second"]`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	nextHost.Store(&server.URL)
	pageTwo.Store(http.StatusOK)
	if got, err := GetPages[string](context.Background(), client, "/rules/branches/main"); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetPages = %v, %v; want both pages", got, err)
	}

	pageTwo.Store(http.StatusForbidden)
	_, err := GetPages[string](context.Background(), client, "/rules/branches/main")
	var answer *Answer
	if !errors.As(err, &answer) || answer.Path != "/repositories/42/rules/branches/main" || answer.Status != http.StatusForbidden {
		t.Fatalf("GetPages with page 2 refused = %v, want an *Answer naming /repositories/42/rules/branches/main with 403", err)
	}

	nextHost.Store(&other.URL)
	got, err := GetPages[string](context.Background(), client, "/rules/branches/main")
	if err == nil || !strings.Contains(err.Error(), other.URL) || got != nil {
		t.Fatalf("GetPages with a next page on another host = %v, %v; want an error naming it and no items", got, err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host saw %d requests, want none", n)
	}
}
