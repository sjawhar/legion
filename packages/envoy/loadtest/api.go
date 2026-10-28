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
	// start is when the client began the request; wrote is when the whole request was in the
	// socket (httptrace WroteRequest), 0 when it never got there; end is when the response body
	// was read.
	start, wrote, end int64
	// connWait is the client's own wait for a connection (httptrace GetConn to GotConn).
	connWait time.Duration
	err      error
}

func (r callResult) ok() bool { return r.err == nil && r.status/100 == 2 }

// arrival is when the request reached Dispatch's socket, or its start when the trace never saw
// the write complete.
func (r callResult) arrival() int64 {
	if r.wrote != 0 {
		return r.wrote
	}
	return r.start
}

// call sends one request and reads the whole response.
func (a *api) call(ctx context.Context, method, path string, w who, body any) callResult {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return callResult{err: err}
		}
		payload = b
	}
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
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.person != "" {
		req.Header.Set("X-Dispatch-User", w.person)
	} else {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	res := callResult{start: time.Now().UnixNano()}
	resp, err := a.client.Do(req)
	if err == nil {
		res.status = resp.StatusCode
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
	for attempt := 0; ; attempt++ {
		res := a.call(ctx, method, path, w, body)
		transient := res.err != nil || res.status == http.StatusTooManyRequests || res.status >= 500
		if res.ok() || !transient || attempt >= 6 || ctx.Err() != nil {
			return res
		}
		time.Sleep(time.Second)
	}
}

func sessionActor(id string) map[string]string { return map[string]string{"kind": "session", "id": id} }

func trunc(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}
