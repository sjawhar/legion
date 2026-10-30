package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// api is a Dispatch HTTP client: agent requests use the stack's shared bearer and name a session
// actor in a write's body; a person's request names the person in the trusted identity header
// (DISPATCH_IDENTITY=header:X-Dispatch-User).
type api struct {
	base   string
	token  string
	client *http.Client
}

func newAPI(base, token string, maxIdle int) *api {
	tr := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
	}
	return &api{base: strings.TrimRight(base, "/"), token: token, client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}
}

// who is a request's identity: a person through the identity header when person is set, the
// agent bearer otherwise.
type who struct {
	person string
}

// callResult is one request's outcome. Times are wall-clock Unix nanoseconds, so they compare
// with receipt times and Postgres commit timestamps on the same box.
type callResult struct {
	status int
	body   []byte
	// etag is the answer's ETag header, empty when it carries none.
	etag string
	// start is when the client began the request; wrote is when the whole request was in the
	// socket (httptrace WroteRequest), 0 when it never got there; end is when the response body
	// was read.
	start, wrote, end int64
	// connWait is the client's own wait for a connection (httptrace GetConn to GotConn).
	connWait time.Duration
	err      error
}

func (r callResult) ok() bool { return r.err == nil && r.status/100 == 2 }

// errorCode is the code a non-2xx answer's body names; "" for a 2xx answer or a transport error.
func (r callResult) errorCode() string {
	if r.err != nil || r.status/100 == 2 {
		return ""
	}
	return errorCode(r.body)
}

// transient is a failure worth retrying: a transport error, 429 or 5xx.
func (r callResult) transient() bool {
	return r.err != nil || r.status == http.StatusTooManyRequests || r.status >= 500
}

// arrival is when the request reached Dispatch's socket, or its start when the trace never saw
// the write complete.
func (r callResult) arrival() int64 {
	if r.wrote != 0 {
		return r.wrote
	}
	return r.start
}

// call sends one request with a JSON body (none when body is nil) and reads the whole response.
func (a *api) call(ctx context.Context, method, path string, w who, body any) callResult {
	return a.callKeyed(ctx, method, path, w, body, "")
}

// callKeyed is call under an idempotency key (contracts.IdempotencyKeyHeader): Dispatch runs the
// write of a key once and replays its stored answer to every later request under that key, so a
// retry of a write whose answer was lost adds no second version. An empty key sends none, which
// is what every unkeyed caller does.
func (a *api) callKeyed(ctx context.Context, method, path string, w who, body any, key string) callResult {
	if body == nil {
		return a.callRawKeyed(ctx, method, path, w, "", nil, key)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return callResult{err: err}
	}
	return a.callRawKeyed(ctx, method, path, w, "application/json", payload, key)
}

// callRaw sends one request whose body is payload under contentType (a multipart upload, say);
// a nil payload sends no body and no Content-Type.
func (a *api) callRaw(ctx context.Context, method, path string, w who, contentType string, payload []byte) callResult {
	return a.callRawKeyed(ctx, method, path, w, contentType, payload, "")
}

// callRawKeyed is callRaw under an idempotency key; an empty key sends none.
func (a *api) callRawKeyed(ctx context.Context, method, path string, w who, contentType string, payload []byte, key string) callResult {
	var getConn, gotConn, wrote atomic.Int64
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { getConn.Store(time.Now().UnixNano()) },
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(time.Now().UnixNano()) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(time.Now().UnixNano())
			}
		},
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, a.base+path, rdr)
	if err != nil {
		return callResult{err: err}
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if w.person != "" {
		req.Header.Set("X-Dispatch-User", w.person)
	} else {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	if key != "" {
		req.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	res := callResult{start: time.Now().UnixNano()}
	resp, err := a.client.Do(req)
	if err == nil {
		res.status = resp.StatusCode
		res.etag = resp.Header.Get("ETag")
		res.body, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	res.end = time.Now().UnixNano()
	res.err = err
	res.wrote = wrote.Load()
	if g, h := getConn.Load(), gotConn.Load(); g != 0 && h >= g {
		res.connWait = time.Duration(h - g)
	}
	return res
}

// callRetry retries a transient failure (a transport error, 429 or 5xx) once a second, up to six
// times, as the seed always has; any other answer returns at once.
func (a *api) callRetry(ctx context.Context, method, path string, w who, body any) callResult {
	return a.callRetryKeyed(ctx, method, path, w, body, "")
}

// callRetryKeyed is callRetry under an idempotency key. A repeat that reaches Dispatch while the
// key's first request still runs is 409 KEY_IN_FLIGHT, which this retries too: under a key that
// answer says the write is running once, not that it must be sent again.
func (a *api) callRetryKeyed(ctx context.Context, method, path string, w who, body any, key string) callResult {
	for attempt := 0; ; attempt++ {
		res := a.callKeyed(ctx, method, path, w, body, key)
		transient := res.transient() ||
			(key != "" && res.status == http.StatusConflict && errorCode(res.body) == contracts.IdempotencyKeyInFlight)
		if res.ok() || !transient || attempt >= 6 || ctx.Err() != nil {
			return res
		}
		time.Sleep(time.Second)
	}
}

// errorCode is the code a Dispatch error body names (writeError writes {"error": ..., "code": ...});
// "" when the body names none or is not JSON at all.
func errorCode(body []byte) string {
	var refusal struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &refusal)
	return refusal.Code
}

// sessionActor is a write's actor body field for a session id, the form a request body and
// multipartFile's form both take.
func sessionActor(id string) map[string]any { return map[string]any{"kind": "session", "id": id} }

func trunc(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}
