package ghnotify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

// t0 is every test's start: the poller's first poll, and the fake GitHub's clock.
var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

// fakeClock is the time both the poller (Config.Now) and the fake GitHub (its Date header) read.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(at time.Time) *fakeClock { return &fakeClock{now: at} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// syncBuffer is a log destination the poller's four thread workers may write to at once.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// lines returns the JSON log records at level whose msg is msg.
func (s *syncBuffer) lines(t *testing.T, level, msg string) []map[string]any {
	t.Helper()
	s.mu.Lock()
	text := s.b.String()
	s.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		if record["level"] == level && record["msg"] == msg {
			out = append(out, record)
		}
	}
	return out
}

// fakePublisher stores envelopes as the notification stream does: under the message id
// DedupeKey:Topic, reporting a second envelope with a stored id as a duplicate.
type fakePublisher struct {
	mu         sync.Mutex
	stored     map[string]contracts.Envelope
	order      []contracts.Envelope
	calls      int
	duplicates int
}

func newFakePublisher() *fakePublisher {
	return &fakePublisher{stored: map[string]contracts.Envelope{}}
}

func (p *fakePublisher) PublishReportingDuplicate(item contracts.Envelope) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	id := item.DedupeKey + ":" + item.Topic
	if _, ok := p.stored[id]; ok {
		p.duplicates++
		return true, nil
	}
	p.stored[id] = item
	p.order = append(p.order, item)
	return false, nil
}

func (p *fakePublisher) storedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.stored)
}

func (p *fakePublisher) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakePublisher) duplicateCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.duplicates
}

// get returns the stored envelope of one change on one topic.
func (p *fakePublisher) get(sourceID, topic string) (contracts.Envelope, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	item, ok := p.stored["github."+sourceID+":"+topic]
	return item, ok
}

// sourceIDs returns the source event ids of the stored envelopes, in the order they were stored.
func (p *fakePublisher) sourceIDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.order))
	for _, item := range p.order {
		out = append(out, item.SourceEventID)
	}
	return out
}

// metricValue reads one series from the registry's exposition, 0 when it has no line.
func metricValue(t *testing.T, reg *metrics.Registry, series string) int64 {
	t.Helper()
	for _, line := range scrape(t, reg) {
		cut := strings.LastIndex(line, " ")
		if cut < 0 || line[:cut] != series {
			continue
		}
		value, err := strconv.ParseInt(line[cut+1:], 10, 64)
		if err != nil {
			t.Fatalf("metric line %q: %v", line, err)
		}
		return value
	}
	return 0
}

// metricSum adds up every series of one metric.
func metricSum(t *testing.T, reg *metrics.Registry, name string) int64 {
	t.Helper()
	var total int64
	for _, line := range scrape(t, reg) {
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		value, err := strconv.ParseInt(line[strings.LastIndex(line, " ")+1:], 10, 64)
		if err != nil {
			t.Fatalf("metric line %q: %v", line, err)
		}
		total += value
	}
	return total
}

func scrape(t *testing.T, reg *metrics.Registry) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return strings.Split(rec.Body.String(), "\n")
}

type recordedRequest struct {
	method string
	path   string
	query  url.Values
	header http.Header
}

// fakeAnswer is a canned answer for one path: status and headers, for times requests, or every
// request when times is negative.
type fakeAnswer struct {
	status int
	header http.Header
	times  int
}

// fakeGitHub serves the Notifications API and the REST lists the poller reads, from objects the
// test adds. It answers 405 to every method but GET and records each such request; a test fails at
// cleanup when it recorded one (A5).
type fakeGitHub struct {
	t     *testing.T
	clock *fakeClock
	srv   *httptest.Server

	mu             sync.Mutex
	threads        map[string]map[string]any
	modified       time.Time
	issues         map[string]map[string]any
	pulls          map[string]map[string]any
	comments       map[string][]map[string]any
	reviewComments map[string][]map[string]any
	reviews        map[string][]map[string]any
	events         map[string][]map[string]any
	pollInterval   string
	rateRemaining  int
	rateReset      time.Time
	header         http.Header
	answers        map[string]*fakeAnswer
	requests       []recordedRequest
	others         []recordedRequest
}

func newFakeGitHub(t *testing.T, clock *fakeClock) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{
		t:              t,
		clock:          clock,
		threads:        map[string]map[string]any{},
		issues:         map[string]map[string]any{},
		pulls:          map[string]map[string]any{},
		comments:       map[string][]map[string]any{},
		reviewComments: map[string][]map[string]any{},
		reviews:        map[string][]map[string]any{},
		events:         map[string][]map[string]any{},
		pollInterval:   "60",
		rateRemaining:  4500,
		rateReset:      clock.Now().Add(time.Hour),
		header:         http.Header{},
		answers:        map[string]*fakeAnswer{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.touch()
	t.Cleanup(func() {
		f.srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, other := range f.others {
			t.Errorf("the poller sent %s %s; it may send only GET", other.method, other.path)
		}
	})
	return f
}

func key(owner, repo string, number int) string { return fmt.Sprintf("%s/%s#%d", owner, repo, number) }

func restRepository(owner, repo string) map[string]any {
	return map[string]any{
		"id":        1000,
		"name":      repo,
		"full_name": owner + "/" + repo,
		"private":   false,
		"owner":     map[string]any{"login": owner, "type": "Organization"},
		"html_url":  "https://github.com/" + owner + "/" + repo,
	}
}

// addThread adds or replaces one participating notification thread and moves the list's
// Last-Modified, so the next conditional list request is answered 200.
func (f *fakeGitHub) addThread(id, owner, repo, typ string, number int, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := map[string]string{"Issue": "issues", "PullRequest": "pulls"}[typ]
	if kind == "" {
		kind = "releases"
	}
	subject := fmt.Sprintf("%s/repos/%s/%s/%s/%d", f.srv.URL, owner, repo, kind, number)
	f.threads[id] = map[string]any{
		"id":         id,
		"unread":     true,
		"reason":     "comment",
		"updated_at": stamp(updated),
		"subject": map[string]any{
			"title": "Thread " + id, "url": subject, "latest_comment_url": subject, "type": typ,
		},
		"repository": restRepository(owner, repo),
		"url":        f.srv.URL + "/notifications/threads/" + id,
	}
	f.touchLocked()
}

// bumpThread moves one thread's updated_at, as a new comment on it does.
func (f *fakeGitHub) bumpThread(id string, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads[id]["updated_at"] = stamp(updated)
	f.touchLocked()
}

func (f *fakeGitHub) addIssue(owner, repo string, number int, pullRequest bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	htmlKind := "issues"
	if pullRequest {
		htmlKind = "pull"
	}
	htmlURL := fmt.Sprintf("https://github.com/%s/%s/%s/%d", owner, repo, htmlKind, number)
	issue := map[string]any{
		"id": 3000 + number, "number": number, "title": fmt.Sprintf("Change %d", number), "state": "open",
		"html_url": htmlURL, "user": map[string]any{"login": "author"}, "body": "What the change is.",
	}
	if pullRequest {
		issue["pull_request"] = map[string]any{"url": fmt.Sprintf("%s/repos/%s/%s/pulls/%d", f.srv.URL, owner, repo, number), "html_url": htmlURL}
		f.pulls[key(owner, repo, number)] = map[string]any{
			"id": 4000 + number, "number": number, "title": fmt.Sprintf("Change %d", number), "state": "open",
			"html_url": htmlURL, "user": map[string]any{"login": "author"}, "body": "What the change is.",
			"head":   map[string]any{"ref": "feature", "sha": "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"},
			"base":   map[string]any{"ref": "main", "sha": "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"},
			"merged": false, "merged_by": nil, "merge_commit_sha": "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00",
			"updated_at": stamp(f.clock.Now()),
		}
	}
	f.issues[key(owner, repo, number)] = issue
}

func (f *fakeGitHub) setMerged(owner, repo string, number int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pull := f.pulls[key(owner, repo, number)]
	pull["state"] = "closed"
	pull["merged"] = true
	pull["merged_by"] = map[string]any{"login": "maintainer"}
	pull["merge_commit_sha"] = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"
}

func (f *fakeGitHub) addComment(owner, repo string, number, id int, user, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(owner, repo, number)
	f.comments[k] = append(f.comments[k], map[string]any{
		"id": id, "user": map[string]any{"login": user}, "body": body,
		"created_at": stamp(at), "updated_at": stamp(at),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/issues/%d#issuecomment-%d", owner, repo, number, id),
	})
}

func (f *fakeGitHub) addReviewComment(owner, repo string, number, id int, user, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(owner, repo, number)
	f.reviewComments[k] = append(f.reviewComments[k], map[string]any{
		"id": id, "user": map[string]any{"login": user}, "body": body, "path": "src/spin.go", "line": 12,
		"original_line": 12, "created_at": stamp(at), "updated_at": stamp(at),
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d#discussion_r%d", owner, repo, number, id),
	})
}

// addReview adds a review in the REST API's upper-case state; a PENDING review has no submitted_at,
// as GitHub answers it.
func (f *fakeGitHub) addReview(owner, repo string, number, id int, user, state string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(owner, repo, number)
	review := map[string]any{
		"id": id, "user": map[string]any{"login": user}, "body": "Review " + strconv.Itoa(id), "state": state,
		"commit_id": "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
		"html_url":  fmt.Sprintf("https://github.com/%s/%s/pull/%d#pullrequestreview-%d", owner, repo, number, id),
	}
	if state != "PENDING" {
		review["submitted_at"] = stamp(at)
	}
	f.reviews[k] = append(f.reviews[k], review)
}

func (f *fakeGitHub) addEvent(owner, repo string, number, id int, event string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(owner, repo, number)
	f.events[k] = append(f.events[k], map[string]any{
		"id": id, "event": event, "actor": map[string]any{"login": "maintainer"}, "created_at": stamp(at),
	})
}

// answer makes the next times requests for path (every one, when times is negative) get status and
// header instead of the path's own answer.
func (f *fakeGitHub) answer(path string, status int, header http.Header, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = &fakeAnswer{status: status, header: header, times: times}
}

func (f *fakeGitHub) clearAnswer(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.answers, path)
}

func (f *fakeGitHub) setPollInterval(seconds string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollInterval = seconds
}

func (f *fakeGitHub) setRate(remaining int, reset time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rateRemaining = remaining
	f.rateReset = reset
}

// setHeader adds a header to every answer.
func (f *fakeGitHub) setHeader(name, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.header.Set(name, value)
}

// touch moves the list's Last-Modified without changing a thread.
func (f *fakeGitHub) touch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touchLocked()
}

func (f *fakeGitHub) touchLocked() {
	next := f.clock.Now().UTC().Truncate(time.Second)
	if !next.After(f.modified) {
		next = f.modified.Add(time.Second)
	}
	f.modified = next
}

// mark is the number of GET requests so far, for requestsSince.
func (f *fakeGitHub) mark() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGitHub) requestsSince(mark int) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests[mark:]...)
}

// detailsSince is every request since mark but the notification list's.
func (f *fakeGitHub) detailsSince(mark int) []recordedRequest {
	var out []recordedRequest
	for _, request := range f.requestsSince(mark) {
		if request.path != "/notifications" {
			out = append(out, request)
		}
	}
	return out
}

func countPath(requests []recordedRequest, path string) int {
	n := 0
	for _, request := range requests {
		if request.path == path {
			n++
		}
	}
	return n
}

// takeOthers returns and forgets the requests that were not GET.
func (f *fakeGitHub) takeOthers() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.others
	f.others = nil
	return out
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request := recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone()}
	if r.Method != http.MethodGet {
		f.others = append(f.others, request)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f.requests = append(f.requests, request)
	h := w.Header()
	h.Set("Date", f.clock.Now().UTC().Format(http.TimeFormat))
	if f.rateRemaining >= 0 {
		h.Set("X-RateLimit-Limit", "5000")
		h.Set("X-RateLimit-Remaining", strconv.Itoa(f.rateRemaining))
		h.Set("X-RateLimit-Reset", strconv.FormatInt(f.rateReset.Unix(), 10))
	}
	for name, values := range f.header {
		h[name] = values
	}
	if r.URL.Path == "/notifications" && f.pollInterval != "" {
		h.Set("X-Poll-Interval", f.pollInterval)
	}
	if answer := f.answers[r.URL.Path]; answer != nil && answer.times != 0 {
		if answer.times > 0 {
			answer.times--
		}
		for name, values := range answer.header {
			h[http.CanonicalHeaderKey(name)] = values
		}
		h.Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = io.WriteString(w, `{"message":"fake answer"}`)
		return
	}
	if r.URL.Path == "/notifications" {
		f.serveNotifications(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/repos/") || len(parts) < 4 || len(parts) > 5 {
		f.notFound(w)
		return
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil {
		f.notFound(w)
		return
	}
	k := key(parts[0], parts[1], number)
	route := parts[2]
	if len(parts) == 5 {
		route += "/" + parts[4]
	}
	switch route {
	case "issues":
		f.serveObject(w, f.issues[k])
	case "pulls":
		f.serveObject(w, f.pulls[k])
	case "issues/comments":
		f.serveList(w, r, updatedSince(r, f.comments[k]))
	case "pulls/comments":
		f.serveList(w, r, updatedSince(r, f.reviewComments[k]))
	case "pulls/reviews":
		f.serveList(w, r, f.reviews[k])
	case "issues/events":
		f.serveList(w, r, f.events[k])
	default:
		f.notFound(w)
	}
}

func (f *fakeGitHub) notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"message":"Not Found"}`)
}

func (f *fakeGitHub) serveObject(w http.ResponseWriter, object map[string]any) {
	if object == nil {
		f.notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(object)
}

// updatedSince keeps the items updated at or after the request's since, as GitHub's comment lists do.
func updatedSince(r *http.Request, items []map[string]any) []map[string]any {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return items
	}
	since, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, item := range items {
		updated, _ := time.Parse(time.RFC3339, item["updated_at"].(string))
		if !updated.Before(since) {
			out = append(out, item)
		}
	}
	return out
}

// serveList answers one page of items as GitHub does: per_page and page, and a Link header naming
// the next and last pages while there are more.
func (f *fakeGitHub) serveList(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	query := r.URL.Query()
	perPage := 30
	if value, err := strconv.Atoi(query.Get("per_page")); err == nil && value > 0 {
		perPage = value
	}
	page := 1
	if value, err := strconv.Atoi(query.Get("page")); err == nil && value > 0 {
		page = value
	}
	pages := (len(items) + perPage - 1) / perPage
	if pages == 0 {
		pages = 1
	}
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	link := func(to int, rel string) string {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(to))
		return fmt.Sprintf("<%s%s?%s>; rel=%q", f.srv.URL, r.URL.Path, q.Encode(), rel)
	}
	var links []string
	if page < pages {
		links = append(links, link(page+1, "next"), link(pages, "last"))
	}
	if page > 1 {
		links = append(links, link(1, "first"), link(page-1, "prev"))
	}
	if len(links) > 0 {
		w.Header().Set("Link", strings.Join(links, ", "))
	}
	w.Header().Set("Content-Type", "application/json")
	out := items[start:end]
	if out == nil {
		out = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeGitHub) serveNotifications(w http.ResponseWriter, r *http.Request) {
	modified := f.modified.Format(http.TimeFormat)
	w.Header().Set("Last-Modified", modified)
	if r.Header.Get("If-Modified-Since") == modified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	var items []map[string]any
	for _, thread := range f.threads {
		updated, _ := time.Parse(time.RFC3339, thread["updated_at"].(string))
		if !updated.Before(since) {
			items = append(items, thread)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i]["updated_at"].(string), items[j]["updated_at"].(string)
		if a != b {
			return a > b
		}
		return items[i]["id"].(string) < items[j]["id"].(string)
	})
	f.serveList(w, r, items)
}

// newTestPoller is a poller on the fake GitHub and its clock, logging to logs and counting on reg.
func newTestPoller(t *testing.T, f *fakeGitHub, pub Publisher, logs *syncBuffer, reg *metrics.Registry, mutate func(*Config)) *Poller {
	t.Helper()
	cfg := Config{Token: "ghp_test", BaseURL: f.srv.URL, HTTPClient: f.srv.Client(), Now: f.clock.Now}
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := New(cfg, pub, logging.NewWithWriter("test", logs), reg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

type harness struct {
	f     *fakeGitHub
	clock *fakeClock
	p     *Poller
	pub   *fakePublisher
	logs  *syncBuffer
	reg   *metrics.Registry
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	clock := newFakeClock(t0)
	h := &harness{f: newFakeGitHub(t, clock), clock: clock, pub: newFakePublisher(), logs: &syncBuffer{}, reg: metrics.New()}
	h.p = newTestPoller(t, h.f, h.pub, h.logs, h.reg, mutate)
	return h
}
