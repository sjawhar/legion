package agentloop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; and makes the
// run's one retry, on a 429, a 5xx or a transport error, within the run's time.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values
}

// newModelTransport is the transport one Run or one Decide makes its model calls through, over
// the default transport, for the endpoint whose protocol, token and query parameters it carries.
func newModelTransport(endpoint Endpoint) *modelTransport {
	return &modelTransport{base: http.DefaultTransport, kind: endpoint.Kind, token: endpoint.Token, query: endpoint.Query}
}

// client is the HTTP client one Run or one Decide makes its model calls with: this transport,
// and no redirect ever followed. The transport puts the endpoint's credential on every request
// it carries, which would include the one the client makes for a 3xx, and Go strips
// Authorization on a cross-domain redirect before a transport runs, so following one would hand
// the token to whatever server the Location names. A redirect fails the call instead, with an
// error client.Do returns above this transport, so the retry never sees it and the caller is
// told the endpoint redirected rather than left to read an empty stream.
func (t *modelTransport) client() *http.Client {
	return &http.Client{Transport: t, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		// The host alone, since a Location can carry a query: this message names no
		// more of it than the server a credential would have gone to. (Go's own
		// client wraps the error in a *url.Error that names the Location in full.)
		return fmt.Errorf("the %s endpoint answered %s, a redirect to %s, which is never followed",
			t.kind, req.Response.Status, req.URL.Host)
	}}
}

// defaultRetryWait is how long the retry waits when the response names no Retry-After.
const defaultRetryWait = time.Second

func (t *modelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		var key string
		// Azure signs its own calls: the provider's credential middleware sets the header,
		// and this transport reads no token for it.
		if t.kind != AzureAnthropic {
			var err error
			if key, err = t.token(ctx); err != nil {
				return nil, fmt.Errorf("reading the %s token: %w", t.kind, err)
			}
		}
		r := req.Clone(ctx)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		switch t.kind {
		case AnthropicMessages:
			r.Header.Set("X-Api-Key", key)
		case OpenAIResponses, OpenAIChat, SystemOne:
			r.Header.Set("Authorization", "Bearer "+key)
		}
		if len(t.query) > 0 {
			q := r.URL.Query()
			for name, values := range t.query {
				// A parameter the provider itself put there is the provider's.
				if _, ok := q[name]; !ok {
					q[name] = values
				}
			}
			r.URL.RawQuery = q.Encode()
		}
		resp, err := t.base.RoundTrip(r)
		if wait, retry := retryable(resp, err); retry && attempt == 0 && waitable(ctx, wait) {
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// retryable is whether a model call's outcome is retried, and after how long: a transport error,
// 429 or a 5xx, after the response's Retry-After (seconds) or a second.
func retryable(resp *http.Response, err error) (time.Duration, bool) {
	if err != nil {
		return defaultRetryWait, !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return 0, false
	}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	return defaultRetryWait, true
}

// waitable is whether the context has the time to wait out a retry: it is not already done, and
// it either carries no deadline or has more than wait left.
func waitable(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > wait
}
