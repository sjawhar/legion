package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAssetStore answers from assets, or with err, and records every key it is asked for.
type fakeAssetStore struct {
	assets map[string]string
	err    error
	keys   []string
}

func (s *fakeAssetStore) GetAsset(_ context.Context, key string) ([]byte, error) {
	s.keys = append(s.keys, key)
	if s.err != nil {
		return nil, s.err
	}
	asset, ok := s.assets[key]
	if !ok {
		return nil, ErrAssetNotFound
	}
	return []byte(asset), nil
}

// A local miss under /assets/ is answered from the asset store, and nothing else is: a local file
// wins, pages and other paths never consult it, and a router with no store answers every asset
// miss with a plain 404. Only a hit carries the immutable header.
func TestStaticHandlerRetainedAssets(t *testing.T) {
	const retained = "console.log('previous build')"
	for _, tc := range []struct {
		name         string
		method       string
		path         string
		store        *fakeAssetStore // nil: the router has no asset store
		status       int
		body         string
		cacheControl string
		contentType  string
		keys         []string
	}{
		{
			name: "retained hit", path: "/assets/previous-build.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/previous-build.js": retained}},
			status: http.StatusOK, body: retained, cacheControl: assetCacheControl,
			contentType: "text/javascript; charset=utf-8", keys: []string{"assets/previous-build.js"},
		},
		{
			name: "retained hit by HEAD", method: http.MethodHead, path: "/assets/previous-build.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/previous-build.js": retained}},
			status: http.StatusOK, cacheControl: assetCacheControl,
			contentType: "text/javascript; charset=utf-8", keys: []string{"assets/previous-build.js"},
		},
		{
			name: "local file wins", path: "/assets/current.js",
			store:  &fakeAssetStore{assets: map[string]string{"assets/current.js": "retained"}},
			status: http.StatusOK, body: "current", cacheControl: assetCacheControl,
		},
		{
			name: "absent object", path: "/assets/never-retained.js", store: &fakeAssetStore{},
			status: http.StatusNotFound, keys: []string{"assets/never-retained.js"},
		},
		{
			name: "store failure", path: "/assets/previous-build.js",
			store:  &fakeAssetStore{err: errors.New("object store unavailable")},
			status: http.StatusBadGateway, keys: []string{"assets/previous-build.js"},
		},
		{
			name: "no store", path: "/assets/previous-build.js",
			status: http.StatusNotFound,
		},
		{
			name: "non-asset file", path: "/previous-build.js",
			store:  &fakeAssetStore{assets: map[string]string{"previous-build.js": retained}},
			status: http.StatusNotFound,
		},
		{
			name: "asset root itself", path: "/assets",
			store:  &fakeAssetStore{assets: map[string]string{"assets": retained}},
			status: http.StatusNotFound,
		},
		{
			name: "page", path: "/issues/CORE-1",
			store:  &fakeAssetStore{assets: map[string]string{"issues/CORE-1": retained}},
			status: http.StatusOK, body: "<!doctype html>", cacheControl: pageCacheControl,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			webDist := t.TempDir()
			if err := os.MkdirAll(filepath.Join(webDist, "assets"), 0o700); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{"index.html": "<!doctype html>", "assets/current.js": "current"} {
				if err := os.WriteFile(filepath.Join(webDist, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			handler, context := newTestRouter(t)
			context.WebDistDir = webDist
			if tc.store != nil {
				context.AssetStore = tc.store
			}
			server := httptest.NewServer(handler)
			defer server.Close()

			method := tc.method
			if method == "" {
				method = http.MethodGet
			}
			request, err := http.NewRequest(method, server.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("%s %s: %v", method, tc.path, err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}

			if response.StatusCode != tc.status || (tc.body != "" && string(body) != tc.body) {
				t.Fatalf("%s %s: status %d body %q, want %d %q", method, tc.path, response.StatusCode, body, tc.status, tc.body)
			}
			if got := response.Header.Get("Cache-Control"); got != tc.cacheControl {
				t.Errorf("Cache-Control %q, want %q", got, tc.cacheControl)
			}
			if tc.contentType != "" {
				if got := response.Header.Get("Content-Type"); got != tc.contentType {
					t.Errorf("Content-Type %q, want %q", got, tc.contentType)
				}
			}
			if method == http.MethodHead {
				if len(body) != 0 || response.Header.Get("Content-Length") != strconv.Itoa(len(retained)) {
					t.Errorf("HEAD: %d body bytes, Content-Length %q, want none and %d", len(body), response.Header.Get("Content-Length"), len(retained))
				}
			}
			var keys []string
			if tc.store != nil {
				keys = tc.store.keys
			}
			if !slices.Equal(keys, tc.keys) {
				t.Errorf("store asked for %q, want %q", keys, tc.keys)
			}
		})
	}
}

// slowClient is a browser on a slow link: each write it is sent waits before it lands.
type slowClient struct {
	*httptest.ResponseRecorder
	delay time.Duration
}

func (c slowClient) Write(p []byte) (int, error) {
	time.Sleep(c.delay)
	return c.ResponseRecorder.Write(p)
}

// The fetch bound is the store's, not the browser's: a client that takes longer than the bound to
// receive a retained asset the store answered at once still receives all of it, as it would the
// same file served from the local build.
func TestStaticHandlerDeliversAWholeRetainedAssetToASlowClient(t *testing.T) {
	body := bytes.Repeat([]byte("/"), 1<<20)
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = &countingStore{object: body}

	recorder := httptest.NewRecorder()
	// 32 KiB copies at 120 ms each take about 3.8 s for 1 MiB, past the store's 3 s bound.
	handler.ServeHTTP(slowClient{ResponseRecorder: recorder, delay: 120 * time.Millisecond},
		httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusOK || recorder.Body.Len() != len(body) {
		t.Fatalf("slow client: status %d, received %d of %d bytes", recorder.Code, recorder.Body.Len(), len(body))
	}
}

// A store that stops answering until the shared fetch bound runs out is the same refusal the memory
// cap gives, 503, not a 502 store failure: the store was never shown to be broken, only asked too
// late to answer, and the client never gets a 200 carrying a year's immutable caching.
func TestStaticHandlerAnswers503ForARetainedAssetThatStallsPastTheFetchBound(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = latentStore{delay: time.Hour, object: []byte("never")}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("stalled store: status %d with %d body bytes, want 503", recorder.Code, recorder.Body.Len())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("stalled store: Cache-Control %q, want no-store", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("stalled store: Retry-After %q, want 1", got)
	}
}

// A failing store is otherwise invisible from inside the task: the browser fails the chunk as it
// would a 404. Each failure leaves one ERROR record naming the key and the store's error.
func TestStaticHandlerLogsARetainedAssetStoreFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = &fakeAssetStore{err: errors.New("object store unavailable")}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("store failure: status %d, want 502", recorder.Code)
	}
	var failures int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Key   string `json:"key"`
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "ERROR" &&
			record.Key == "assets/previous-build.js" && strings.Contains(record.Error, "object store unavailable") {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("store failure: %d ERROR records naming the key and the error, want 1; log %q", failures, logs.String())
	}
}

// countingStore answers every key with one shared object and counts the calls.
type countingStore struct {
	object []byte
	calls  atomic.Int32
}

func (s *countingStore) GetAsset(context.Context, string) ([]byte, error) {
	s.calls.Add(1)
	return s.object, nil
}

// stalledClient is a browser that stops reading: its first write blocks until the client goes
// away, and then fails, as a write to a closed connection does.
type stalledClient struct {
	header  http.Header
	status  int
	writing chan struct{} // closed at the first write
	gone    chan struct{} // closed when the client goes away
	served  chan struct{} // closed when the handler returns
	once    sync.Once
}

func (c *stalledClient) Header() http.Header    { return c.header }
func (c *stalledClient) WriteHeader(status int) { c.status = status }
func (c *stalledClient) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.writing) })
	<-c.gone
	return 0, errors.New("client went away")
}

// stallRetainedReaders starts n clients that each get a retained asset and stop reading it,
// one at a time, and returns once all n are writing. Each goes away when the test ends.
func stallRetainedReaders(t *testing.T, handler http.Handler, n int) []*stalledClient {
	t.Helper()
	clients := make([]*stalledClient, n)
	for i := range clients {
		client := &stalledClient{header: http.Header{}, writing: make(chan struct{}), gone: make(chan struct{}), served: make(chan struct{})}
		clients[i] = client
		go func() {
			defer close(client.served)
			handler.ServeHTTP(client, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/assets/stalled-%d.js", i), nil))
		}()
		<-client.writing
		if client.status != http.StatusOK {
			t.Fatalf("stalled reader %d: status %d, want 200", i, client.status)
		}
	}
	t.Cleanup(func() {
		for _, client := range clients {
			select {
			case <-client.gone:
			default:
				close(client.gone)
			}
			<-client.served
		}
	})
	return clients
}

// A retained object stays in memory until its client has read it, and nothing bounds how long a
// client takes, so the bytes held at once are capped. A request that finds no room before its
// fetch bound runs out is refused without asking the store: 503, not kept, retried after a second.
func TestStaticHandlerCapsRetainedBytesHeldAtOnce(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	store := &countingStore{object: bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize))}
	context.AssetStore = store
	stallRetainedReaders(t, handler, int(maxRetainedBytesHeld/MaxRetainedAssetSize))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/one-more.js", nil))

	if held := int64(store.calls.Load()) * MaxRetainedAssetSize; held > maxRetainedBytesHeld {
		t.Errorf("the store handed out %d bytes still held, past the %d-byte cap", held, maxRetainedBytesHeld)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("past the cap: status %d with %d body bytes, want 503", recorder.Code, recorder.Body.Len())
	}
	if got := store.calls.Load(); got != int32(maxRetainedBytesHeld/MaxRetainedAssetSize) {
		t.Errorf("past the cap: the store was asked %d times, want %d (no call for the refused request)", got, maxRetainedBytesHeld/MaxRetainedAssetSize)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("past the cap: Cache-Control %q, want no-store", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("past the cap: Retry-After %q, want 1", got)
	}
	var warnings int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Key   string `json:"key"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "WARN" && record.Key == "assets/one-more.js" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Errorf("past the cap: %d WARN records naming the key, want 1; log %q", warnings, logs.String())
	}
}

// A client that goes away mid-response gives its bytes back, so the next request is served.
func TestStaticHandlerReleasesRetainedBytesWhenAClientGoesAway(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	store := &countingStore{object: bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize))}
	context.AssetStore = store
	clients := stallRetainedReaders(t, handler, int(maxRetainedBytesHeld/MaxRetainedAssetSize))

	full := httptest.NewRecorder()
	handler.ServeHTTP(full, httptest.NewRequest(http.MethodGet, "/assets/while-full.js", nil))
	if full.Code != http.StatusServiceUnavailable {
		t.Fatalf("at the cap: status %d, want 503", full.Code)
	}

	close(clients[0].gone)
	<-clients[0].served

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/after.js", nil))
	if recorder.Code != http.StatusOK || int64(recorder.Body.Len()) != MaxRetainedAssetSize {
		t.Fatalf("after a client went away: status %d with %d body bytes, want 200 with %d", recorder.Code, recorder.Body.Len(), MaxRetainedAssetSize)
	}
}

// A failed read gives back what it reserved: more failures than the cap holds objects, then a hit.
func TestStaticHandlerReleasesRetainedBytesWhenTheStoreFails(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	failing := &fakeAssetStore{err: errors.New("object store unavailable")}
	context.AssetStore = failing
	for i := range int(maxRetainedBytesHeld/MaxRetainedAssetSize) + 1 {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/assets/failing-%d.js", i), nil))
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("failure %d: status %d, want 502", i, recorder.Code)
		}
	}

	context.AssetStore = &fakeAssetStore{assets: map[string]string{"assets/after.js": "after"}}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/after.js", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "after" {
		t.Fatalf("after the store failed: status %d body %q, want 200 \"after\"", recorder.Code, recorder.Body.String())
	}
}

// latentStore answers as a store across a network does: each object arrives after a delay, or the
// read fails with its context's error when that ends first.
type latentStore struct {
	delay  time.Duration
	object []byte
}

func (s latentStore) GetAsset(ctx context.Context, _ string) ([]byte, error) {
	select {
	case <-time.After(s.delay):
		return s.object, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// A stale tab opening a route asks for every chunk the route imports at once (the dashboard's
// issue page imports eight), and another tab can be doing the same. Small chunks hold almost
// nothing of the memory cap, so a burst of them is served rather than refused.
func TestStaticHandlerServesABurstOfSmallRetainedAssets(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	context.AssetStore = latentStore{delay: 50 * time.Millisecond, object: []byte("console.log('previous build')")}

	const burst = 16
	codes := make([]int, burst)
	var wg sync.WaitGroup
	for i := range burst {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/assets/chunk-%d.js", i), nil))
			codes[i] = recorder.Code
		}()
	}
	wg.Wait()

	refused := 0
	for _, code := range codes {
		if code != http.StatusOK {
			refused++
		}
	}
	if refused != 0 {
		t.Fatalf("a burst of %d small retained assets: %d not served (statuses %v), want all 200", burst, refused, codes)
	}
}

// A request keeps only its object's size once it is read, so stalled readers of real chunk sizes
// share the cap rather than each holding the most one object can be: 32 stalled readers of the
// dashboard's largest chunk, 1,747,955 bytes, fit under the cap, and every one is served.
func TestStaticHandlerHoldsOnlyEachRetainedObjectsSize(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	const size, readers = 1747955, 32
	if size*readers > maxRetainedBytesHeld {
		t.Fatalf("%d readers of %d bytes exceed the %d-byte cap; the test needs them to fit", readers, size, maxRetainedBytesHeld)
	}
	context.AssetStore = &countingStore{object: bytes.Repeat([]byte("/"), size)}

	stallRetainedReaders(t, handler, readers)
}

// A fetch that runs out of the shared three-second bound before the store answers — most often
// because most of it was already spent waiting for memory-cap room, leaving the store call only
// milliseconds to finish — is the same 503 the cap's own refusal gives, not a 502 store failure:
// the store was never actually unhealthy, only asked too late to answer in time. The request's own
// context carries a deadline shorter than retainedAssetFetchTimeout, standing in for a bound that
// ran out mostly waiting on the memory cap, so the fetch's shared ctx expires in milliseconds
// rather than the full three seconds.
func TestStaticHandlerAnswers503WhenTheFetchRunsOutOfTheSharedBound(t *testing.T) {
	shortBound, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, appContext := newTestRouter(t)
	appContext.WebDistDir = t.TempDir()
	appContext.AssetStore = latentStore{delay: time.Second, object: []byte("late")}

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/late-joiner.js", nil).WithContext(shortBound)
	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("fetch past the shared bound: status %d with %d body bytes, want 503", recorder.Code, recorder.Body.Len())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("fetch past the shared bound: Cache-Control %q, want no-store", got)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("fetch past the shared bound: Retry-After %q, want 1", got)
	}
	var warnings, errorRecords int
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
			Key   string `json:"key"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || record.Key != "assets/late-joiner.js" {
			continue
		}
		switch record.Level {
		case "WARN":
			warnings++
		case "ERROR":
			errorRecords++
		}
	}
	if warnings != 1 {
		t.Errorf("fetch past the shared bound: %d WARN records naming the key, want 1; log %q", warnings, logs.String())
	}
	if errorRecords != 0 {
		t.Errorf("fetch past the shared bound: %d ERROR records naming the key, want 0; log %q", errorRecords, logs.String())
	}
}

// neverWrittenResponseWriter fails the test the moment anything is written to it: a canceled
// request has nobody left to answer, so the handler must never call WriteHeader or Write.
type neverWrittenResponseWriter struct {
	t      *testing.T
	header http.Header
}

func (w *neverWrittenResponseWriter) Header() http.Header { return w.header }

func (w *neverWrittenResponseWriter) WriteHeader(status int) {
	w.t.Fatalf("client gone while queued: WriteHeader(%d) called, want no response", status)
}

func (w *neverWrittenResponseWriter) Write(p []byte) (int, error) {
	w.t.Fatalf("client gone while queued: Write(%q) called, want no response", p)
	return 0, nil
}

// A client that disconnects while its request is still queued for memory-cap room is not a cap
// refusal: the cap can be entirely empty, there is nobody left to answer, and the handler writes
// nothing and logs no WARN (the cap-refusal line would otherwise double as a false capacity alarm
// on every ordinary aborted page load).
func TestStaticHandlerWritesNoResponseWhenTheClientCancelsWhileQueuedForTheMemoryCap(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, appContext := newTestRouter(t)
	appContext.WebDistDir = t.TempDir()
	appContext.AssetStore = &fakeAssetStore{}

	req := httptest.NewRequest(http.MethodGet, "/assets/aborted.js", nil).WithContext(canceled)
	handler.ServeHTTP(&neverWrittenResponseWriter{t: t, header: http.Header{}}, req)

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && record.Level == "WARN" {
			t.Fatalf("client gone while queued: a WARN was logged, want none; log %q", logs.String())
		}
	}
}

// A navigation cancels the chunks the page was still loading. A request whose client goes away
// while the store reads its object is answered nothing and logs no ERROR: the store did not fail,
// and a store failure line on every navigation would be a false alarm. The read it started still
// finishes, for the next request of the same chunk.
func TestStaticHandlerWritesNoResponseWhenTheClientCancelsWhileTheStoreReads(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, appContext := newTestRouter(t)
	appContext.WebDistDir = t.TempDir()
	appContext.AssetStore = latentStore{delay: 300 * time.Millisecond, object: []byte("console.log('lib')")}
	navigatedAway, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	handler.ServeHTTP(&neverWrittenResponseWriter{t: t, header: http.Header{}},
		httptest.NewRequest(http.MethodGet, "/assets/lib.js", nil).WithContext(navigatedAway))

	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record struct {
			Level string `json:"level"`
		}
		if json.Unmarshal([]byte(line), &record) == nil && (record.Level == "ERROR" || record.Level == "WARN") {
			t.Fatalf("client gone while the store read: a %s was logged, want none; log %q", record.Level, logs.String())
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/lib.js", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "console.log('lib')" {
		t.Fatalf("the next request: status %d body %q, want 200 with the object", recorder.Code, recorder.Body.String())
	}
}

// Every tab of another build asks for the same chunks, and a stale tab asks again on each route it
// opens, so what the store answered once is kept: a second request for it is served from memory
// without asking the store, and without a reservation, so it is served even while stalled readers
// hold every byte of the cap and a miss would be refused.
func TestStaticHandlerServesACachedAssetWithoutTheStore(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	store := &countingStore{object: bytes.Repeat([]byte("/"), int(MaxRetainedAssetSize))}
	context.AssetStore = store
	readers := int(maxRetainedBytesHeld / MaxRetainedAssetSize)
	stallRetainedReaders(t, handler, readers)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/stalled-0.js", nil))

	if recorder.Code != http.StatusOK || int64(recorder.Body.Len()) != MaxRetainedAssetSize {
		t.Fatalf("a cached asset at the cap: status %d with %d body bytes, want 200 with %d", recorder.Code, recorder.Body.Len(), MaxRetainedAssetSize)
	}
	if got := recorder.Header().Get("Cache-Control"); got != assetCacheControl {
		t.Errorf("a cached asset: Cache-Control %q, want %q", got, assetCacheControl)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("a cached asset: Content-Type %q, want text/javascript", got)
	}
	if got := store.calls.Load(); got != int32(readers) {
		t.Errorf("the store was asked %d times, want %d (none for the cached asset)", got, readers)
	}
}

// gatheringStore holds every read until want reads have arrived or wait runs out, and counts the
// reads: requests that each read the store arrive together, while one read they share arrives
// alone and is answered after wait.
type gatheringStore struct {
	object  []byte
	want    int32
	wait    time.Duration
	calls   atomic.Int32
	arrived chan struct{} // closed when the want-th read arrives
}

func (s *gatheringStore) GetAsset(ctx context.Context, _ string) ([]byte, error) {
	if s.calls.Add(1) == s.want {
		close(s.arrived)
	}
	select {
	case <-s.arrived:
	case <-time.After(s.wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.object, nil
}

// A route of a stale tab, or many tabs of one build at once, ask for one chunk together before the
// store has answered any of them: the store is read once and every request is answered with it.
func TestStaticHandlerReadsTheStoreOnceForConcurrentMisses(t *testing.T) {
	handler, context := newTestRouter(t)
	context.WebDistDir = t.TempDir()
	const object = "console.log('another build')"
	requests := int(maxRetainedBytesHeld / MaxRetainedAssetSize)
	store := &gatheringStore{object: []byte(object), want: int32(requests), wait: time.Second, arrived: make(chan struct{})}
	context.AssetStore = store

	recorders := make([]*httptest.ResponseRecorder, requests)
	var wg sync.WaitGroup
	for i := range recorders {
		recorders[i] = httptest.NewRecorder()
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler.ServeHTTP(recorders[i], httptest.NewRequest(http.MethodGet, "/assets/shared.js", nil))
		}()
	}
	wg.Wait()

	for i, recorder := range recorders {
		if recorder.Code != http.StatusOK || recorder.Body.String() != object {
			t.Errorf("request %d: status %d body %q, want 200 %q", i, recorder.Code, recorder.Body.String(), object)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Errorf("%d concurrent misses of one asset read the store %d times, want once", requests, got)
	}
}

// The cache keeps at most maxCachedAssetBytes however much it is handed, and evicts the object used
// least recently: one read again since it was stored stays, so the chunks a tab is still opening
// are not the ones lost.
func TestAssetCacheKeepsAtMostItsLimitEvictingTheLeastRecentlyUsed(t *testing.T) {
	cache := newAssetCache(maxCachedAssetBytes)
	sizes := []int64{MaxRetainedAssetSize, 3 << 20, 1, MaxRetainedAssetSize - 1, 5 << 20, 1747955}
	for i := range 60 {
		cache.put(fmt.Sprintf("assets/chunk-%d.js", i), make([]byte, sizes[i%len(sizes)]))
		var held int64
		for element := cache.order.Front(); element != nil; element = element.Next() {
			held += int64(len(element.Value.(*cachedAsset).content))
		}
		if held > maxCachedAssetBytes || held != cache.held {
			t.Fatalf("after %d objects the cache holds %d bytes and counts %d, want at most %d and the same count", i+1, held, cache.held, maxCachedAssetBytes)
		}
	}

	cache = newAssetCache(maxCachedAssetBytes)
	full := int(maxCachedAssetBytes / MaxRetainedAssetSize)
	for i := range full {
		cache.put(fmt.Sprintf("assets/chunk-%d.js", i), make([]byte, MaxRetainedAssetSize))
	}
	if _, ok := cache.get("assets/chunk-0.js"); !ok {
		t.Fatal("the first object was evicted from a cache it fits in")
	}
	cache.put("assets/one-more.js", make([]byte, MaxRetainedAssetSize))
	if _, ok := cache.get("assets/chunk-0.js"); !ok {
		t.Error("the object read most recently was evicted")
	}
	if _, ok := cache.get("assets/chunk-1.js"); ok {
		t.Error("the object used least recently was kept past the limit")
	}
	if _, ok := cache.get("assets/one-more.js"); !ok {
		t.Error("the newest object was not kept")
	}
}
