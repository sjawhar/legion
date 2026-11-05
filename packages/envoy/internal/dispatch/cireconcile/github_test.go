package cireconcile

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
)

// fakeGitHub is an httptest stand-in for the GitHub App and REST APIs a reconcile pass calls,
// built on the shape of delivery's own newFakeGitHub: it mounts the installation lookup and the
// token mint, and over them the four reads this package makes -- a repository's identity, its
// open pull requests, and a commit's check runs and check suites -- each paged as GitHub pages
// them, each answered with an ETag of its own, and each recorded.
//
// Its ETag is a digest of the body the URL (path and query together) would answer with, so the
// ETag of a URL changes exactly when a test changes what that URL answers, and a request whose
// If-None-Match matches is answered 304 with no body, as GitHub answers one.
//
// Every field is held under mu, so two passes may run against one stand-in.
type fakeGitHub struct {
	t   *testing.T
	mux *http.ServeMux

	mu        sync.Mutex
	installID int64
	mints     int
	// repos answers GET /repos/{owner}/{repo}, keyed by the owner/repo the request names, which
	// is not always the full_name the answer gives: a repository read under a name it has been
	// renamed from answers with the name GitHub gives it now.
	repos map[string]fakeRepository
	// pulls, runs and suites are keyed by the owner/repo the request names, and runs and suites
	// by "owner/repo@sha".
	pulls  map[string][]fakePull
	runs   map[string][]object
	suites map[string][]object
	// etags is the ETag the stand-in last answered each URL with, so a test can read one.
	etags map[string]string
	// requests is every request the stand-in received, oldest first.
	requests []fakeRequest
	// limitEverything answers every request as GitHub's rate limit; limitOnce answers that many
	// more requests to one path so, and no others.
	limitEverything bool
	limitOnce       map[string]int
}

// fakeRepository is what GET /repos/{owner}/{repo} answers: the name GitHub itself gives the
// repository and its id.
type fakeRepository struct {
	fullName string
	id       int64
}

// fakePull is one open pull request of GET /repos/{owner}/{repo}/pulls.
type fakePull struct {
	number int64
	sha    string
}

// fakeRequest is one request the stand-in received: the URL is the path and query together, as
// an ETag is held by.
type fakeRequest struct {
	method      string
	url         string
	ifNoneMatch string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:         t,
		mux:       http.NewServeMux(),
		installID: 1,
		repos:     map[string]fakeRepository{},
		pulls:     map[string][]fakePull{},
		runs:      map[string][]object{},
		suites:    map[string][]object{},
		etags:     map[string]string{},
		limitOnce: map[string]int{},
	}
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, known := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		installID := f.installID
		f.mu.Unlock()
		if !known {
			// GitHub answers 404 for a repository the App is not installed on, which the client
			// reads as ErrNoInstallation.
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.encode(w, map[string]any{
			"id":          installID,
			"app_slug":    "cireconcile-test",
			"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"},
		})
	})
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.mints++
		token := fmt.Sprintf("ghs_fake_%s_%d", r.PathValue("id"), f.mints)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		f.encode(w, map[string]any{"token": token, "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano)})
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		repository, known := f.repos[r.PathValue("owner")+"/"+r.PathValue("repo")]
		f.mu.Unlock()
		if !known {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		f.answer(w, r, map[string]any{"full_name": repository.fullName, "id": repository.id})
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		pulls := f.pulls[r.PathValue("owner")+"/"+r.PathValue("repo")]
		f.mu.Unlock()
		payload := make([]map[string]any, 0, len(pulls))
		for _, pull := range onePage(r, pulls) {
			payload = append(payload, map[string]any{
				"number": pull.number,
				"state":  "open",
				"head":   map[string]any{"sha": pull.sha},
			})
		}
		f.answer(w, r, payload)
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		runs := f.runs[commitKey(r)]
		f.mu.Unlock()
		page := onePage(r, runs)
		f.answer(w, r, map[string]any{"total_count": len(runs), "check_runs": page})
	})
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-suites", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		suites := f.suites[commitKey(r)]
		f.mu.Unlock()
		page := onePage(r, suites)
		f.answer(w, r, map[string]any{"total_count": len(suites), "check_suites": page})
	})
	return f
}

// commitKey is the key a commit's check objects are held under: "owner/repo@sha".
func commitKey(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo") + "@" + r.PathValue("sha")
}

// serve records one request, answers it as a rate limit when a test asked for that, and
// otherwise hands it to the routes above.
func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{
		method:      r.Method,
		url:         r.URL.RequestURI(),
		ifNoneMatch: r.Header.Get("If-None-Match"),
	})
	limited := f.limitEverything
	if left := f.limitOnce[r.URL.Path]; left > 0 {
		f.limitOnce[r.URL.Path] = left - 1
		limited = true
	}
	f.mu.Unlock()
	if limited {
		// GitHub's own answer to an exhausted rate limit.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
		return
	}
	f.mux.ServeHTTP(w, r)
}

// answer writes one JSON answer with the ETag of its body, or 304 and no body when the request
// carries that ETag already.
func (f *fakeGitHub) answer(w http.ResponseWriter, r *http.Request, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		f.t.Errorf("encode the answer to %s: %v", r.URL.RequestURI(), err)
		return
	}
	digest := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(digest[:8]) + `"`
	f.mu.Lock()
	f.etags[r.URL.RequestURI()] = etag
	f.mu.Unlock()
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if _, err := w.Write(body); err != nil {
		f.t.Errorf("write the answer to %s: %v", r.URL.RequestURI(), err)
	}
}

// encode writes one JSON answer that carries no ETag: the App endpoints, which no caller reads
// conditionally.
func (f *fakeGitHub) encode(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		f.t.Errorf("encode an App answer: %v", err)
	}
}

// onePage is the slice of items the request's per_page and page name, as GitHub pages a listing.
func onePage[T any](r *http.Request, items []T) []T {
	perPage := queryNumber(r.URL.Query(), "per_page", 30)
	page := queryNumber(r.URL.Query(), "page", 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	from := (page - 1) * perPage
	if from >= len(items) {
		return nil
	}
	return items[from:min(from+perPage, len(items))]
}

func queryNumber(values url.Values, name string, fallback int) int {
	number, err := strconv.Atoi(values.Get(name))
	if err != nil {
		return fallback
	}
	return number
}

// client is a githubapp.Client on a throwaway key, pointed at this stand-in, obeying and
// recording the shared rate limits as every Dispatch process's client does.
func (f *fakeGitHub) client(limits *ratelimit.Store) *githubapp.Client {
	f.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatalf("generate a fixture key: %v", err)
	}
	text := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(server.Close)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.cireconcile", PEM: text}, server.URL, githubapp.WithLimits(limits))
	if err != nil {
		f.t.Fatalf("new client: %v", err)
	}
	if client == nil {
		f.t.Fatal("new client: got nil for a configured app")
	}
	return client
}

// setRepository makes GET /repos/{name} answer with fullName and id. name is the owner/repo a
// request names it by, which a renamed repository has two of.
func (f *fakeGitHub) setRepository(name, fullName string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[name] = fakeRepository{fullName: fullName, id: id}
}

// setPullRequests makes repository's open pull requests exactly these.
func (f *fakeGitHub) setPullRequests(repository string, pulls ...fakePull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls[repository] = pulls
}

// setCheckRuns makes the check runs of one commit exactly these, which changes the ETag of the
// URL that lists them.
func (f *fakeGitHub) setCheckRuns(repository, sha string, runs ...object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs[repository+"@"+sha] = runs
}

// setCheckSuites makes the check suites of one commit exactly these.
func (f *fakeGitHub) setCheckSuites(repository, sha string, suites ...object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suites[repository+"@"+sha] = suites
}

// rateLimitEverything answers every request from now on as GitHub answers an exhausted limit.
func (f *fakeGitHub) rateLimitEverything() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitEverything = true
}

// rateLimitNext answers the next request to path so, and no other request.
func (f *fakeGitHub) rateLimitNext(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limitOnce[path]++
}

// received is every request the stand-in has been sent, oldest first.
func (f *fakeGitHub) received() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeRequest(nil), f.requests...)
}

// count is how many requests the stand-in has been sent.
func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// countPath is how many of them named path.
func (f *fakeGitHub) countPath(path string) int {
	sent := 0
	for _, request := range f.received() {
		if before, _, _ := strings.Cut(request.url, "?"); before == path {
			sent++
		}
	}
	return sent
}

// etagOf is the ETag the stand-in last answered one URL (path and query together) with.
func (f *fakeGitHub) etagOf(requestURI string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.etags[requestURI]
}
