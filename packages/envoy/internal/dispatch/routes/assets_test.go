package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
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

// A local miss under /assets/ is answered from the retained-asset store, and nothing else is: a
// local file wins, pages and other paths never consult it, and without a store every asset miss is
// the plain 404 it was before. Only a retained hit carries the immutable header.
func TestStaticHandlerRetainedAssets(t *testing.T) {
	const retained = "console.log('previous build')"
	for _, tc := range []struct {
		name         string
		method       string
		path         string
		store        *fakeAssetStore // nil: DISPATCH_ASSET_STORE_BUCKET unset
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
			name: "setting unset", path: "/assets/previous-build.js",
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
			handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
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

// trackedBody is an S3 object body that records whether it was read and closed.
type trackedBody struct {
	io.Reader
	read, closed bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	b.read = true
	return b.Reader.Read(p)
}

func (b *trackedBody) Close() error {
	b.closed = true
	return nil
}

// stubS3 answers GetObject with output or err and records the request.
type stubS3 struct {
	output *s3.GetObjectOutput
	err    error
	input  *s3.GetObjectInput
}

func (s *stubS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	s.input = in
	return s.output, s.err
}

// The S3 adapter decides between a missing asset (404) and a failing store (502), and is the one
// place an object becomes bytes: it reads the whole object or refuses it.
func TestS3AssetStoreGetAsset(t *testing.T) {
	// The SDK wraps every API error in an operation error, as these do.
	operationError := func(err error) error {
		return &smithy.OperationError{ServiceID: "S3", OperationName: "GetObject", Err: err}
	}
	object := func(body string, length *int64) (*s3.GetObjectOutput, *trackedBody) {
		tracked := &trackedBody{Reader: strings.NewReader(body)}
		return &s3.GetObjectOutput{Body: tracked, ContentLength: length}, tracked
	}
	for _, tc := range []struct {
		name string
		// output builds the stub's answer; tracked is its body, nil when it has none.
		output   func() (*s3.GetObjectOutput, *trackedBody)
		err      error
		want     string
		notFound bool
		failure  bool
		unread   bool
	}{
		{
			name:   "object",
			output: func() (*s3.GetObjectOutput, *trackedBody) { return object("chunk", aws.Int64(5)) },
			want:   "chunk",
		},
		{name: "missing key", err: operationError(&types.NoSuchKey{}), notFound: true},
		{
			// Without s3:ListBucket, S3 answers a missing key this way: a store failure, never 404.
			name: "access denied", err: operationError(&smithy.GenericAPIError{Code: "AccessDenied"}), failure: true,
		},
		{
			name: "no body",
			output: func() (*s3.GetObjectOutput, *trackedBody) {
				return &s3.GetObjectOutput{ContentLength: aws.Int64(5)}, nil
			},
			failure: true,
		},
		{
			name:    "no content length",
			output:  func() (*s3.GetObjectOutput, *trackedBody) { return object("chunk", nil) },
			failure: true,
		},
		{
			name:    "over the limit",
			output:  func() (*s3.GetObjectOutput, *trackedBody) { return object("chunk", aws.Int64(maxRetainedAssetSize+1)) },
			failure: true, unread: true,
		},
		{
			name:    "body shorter than declared",
			output:  func() (*s3.GetObjectOutput, *trackedBody) { return object("chu", aws.Int64(5)) },
			failure: true,
		},
		{
			name:    "body longer than declared",
			output:  func() (*s3.GetObjectOutput, *trackedBody) { return object("chunk!", aws.Int64(5)) },
			failure: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubS3{err: tc.err}
			var tracked *trackedBody
			if tc.output != nil {
				stub.output, tracked = tc.output()
			}
			store := &s3AssetStore{client: stub, bucket: "retained"}

			data, err := store.GetAsset(context.Background(), "assets/chunk.js")

			if aws.ToString(stub.input.Bucket) != "retained" || aws.ToString(stub.input.Key) != "assets/chunk.js" {
				t.Errorf("GetObject asked for %s/%s, want retained/assets/chunk.js", aws.ToString(stub.input.Bucket), aws.ToString(stub.input.Key))
			}
			switch {
			case tc.notFound:
				if !errors.Is(err, ErrAssetNotFound) {
					t.Fatalf("err = %v, want ErrAssetNotFound", err)
				}
			case tc.failure:
				if err == nil || errors.Is(err, ErrAssetNotFound) {
					t.Fatalf("data %q, err = %v, want a store failure", data, err)
				}
			default:
				if err != nil || string(data) != tc.want {
					t.Fatalf("data %q, err = %v, want %q", data, err, tc.want)
				}
			}
			if tracked != nil {
				if !tracked.closed {
					t.Error("the object body was left open")
				}
				if tc.unread && tracked.read {
					t.Error("an object over the limit was read")
				}
			}
		})
	}
}

// streamingBody reads as the S3 SDK's response body does: once the context of the GetObject call
// ends, a read fails with that context's error. With stallAfter >= 0 it delivers that many bytes
// and then waits on the context, as a store that stops sending mid-object does.
type streamingBody struct {
	ctx        context.Context
	body       io.Reader
	stallAfter int
	delivered  int
}

func (b *streamingBody) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.stallAfter >= 0 {
		if b.delivered >= b.stallAfter {
			<-b.ctx.Done()
			return 0, b.ctx.Err()
		}
		if remaining := b.stallAfter - b.delivered; len(p) > remaining {
			p = p[:remaining]
		}
	}
	n, err := b.body.Read(p)
	b.delivered += n
	return n, err
}

func (b *streamingBody) Close() error { return nil }

// streamingS3 answers GetObject at once, with a body that reads as streamingBody does.
type streamingS3 struct {
	body       []byte
	stallAfter int
}

func (s streamingS3) GetObject(ctx context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{
		Body:          &streamingBody{ctx: ctx, body: bytes.NewReader(s.body), stallAfter: s.stallAfter},
		ContentLength: aws.Int64(int64(len(s.body))),
	}, nil
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
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = t.TempDir()
	context.AssetStore = &s3AssetStore{client: streamingS3{body: body, stallAfter: -1}, bucket: "retained"}

	recorder := httptest.NewRecorder()
	// 32 KiB copies at 120 ms each take about 3.8 s for 1 MiB, past the store's 3 s bound.
	handler.ServeHTTP(slowClient{ResponseRecorder: recorder, delay: 120 * time.Millisecond},
		httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusOK || recorder.Body.Len() != len(body) {
		t.Fatalf("slow client: status %d, received %d of %d bytes", recorder.Code, recorder.Body.Len(), len(body))
	}
}

// A store that stops sending part-way through an object is a store failure, answered before any
// header is committed: never a 200 carrying a year's immutable caching and a truncated body.
func TestStaticHandlerAnswers502ForARetainedAssetThatStallsMidBody(t *testing.T) {
	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
	context.WebDistDir = t.TempDir()
	context.AssetStore = &s3AssetStore{client: streamingS3{body: bytes.Repeat([]byte("/"), 64), stallAfter: 12}, bucket: "retained"}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/previous-build.js", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("stalled store: status %d with %d body bytes, want 502", recorder.Code, recorder.Body.Len())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("stalled store: Cache-Control %q, want none", got)
	}
}

// A failing store is otherwise invisible from inside the task: the browser fails the chunk as it
// would a 404. Each failure leaves one ERROR record naming the key and the store's error.
func TestStaticHandlerLogsARetainedAssetStoreFailure(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler, context := newTestRouter(t, &memoryUserStore{users: map[string]*auth.User{}}, nil)
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
