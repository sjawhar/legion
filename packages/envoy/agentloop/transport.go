package agentloop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; makes the run's
// one retry, on a 429, a 5xx or a transport error, within the run's time; and keeps each response
// it returns, so an OpenAI Responses turn's output items can be appended byte for byte as the API
// sent them.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values

	mu sync.Mutex
	// responses are the bodies of the responses returned, one per model call, in order.
	responses []*bytes.Buffer
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
		wait, retry := retryable(resp, err)
		if retry && attempt == 0 && ctx.Err() == nil {
			if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > wait {
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
		}
		if err != nil {
			return nil, err
		}
		if resp.StatusCode/100 == 2 {
			kept := &bytes.Buffer{}
			t.mu.Lock()
			t.responses = append(t.responses, kept)
			t.mu.Unlock()
			resp.Body = &keptBody{ReadCloser: resp.Body, kept: kept, mu: &t.mu}
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

// keptBody is a response body that keeps what is read of it.
type keptBody struct {
	io.ReadCloser
	kept *bytes.Buffer
	mu   *sync.Mutex
}

func (b *keptBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.kept.Write(p[:n])
	b.mu.Unlock()
	return n, err
}

// outputItems are the output items of each of the first calls OpenAI Responses calls, in order,
// each byte for byte as its stream's response.output_item.done event carried it.
func (t *modelTransport) outputItems(calls int) ([][]json.RawMessage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.responses) < calls {
		return nil, fmt.Errorf("%d model calls finished but %d responses were kept", calls, len(t.responses))
	}
	items := make([][]json.RawMessage, calls)
	for i := range calls {
		var err error
		if items[i], err = streamItems(t.responses[i].Bytes()); err != nil {
			return nil, fmt.Errorf("model call %d's stream: %w", i+1, err)
		}
	}
	return items, nil
}

// streamItems are the items of a Responses stream's response.output_item.done events.
func streamItems(stream []byte) ([]json.RawMessage, error) {
	var items []json.RawMessage
	var data bytes.Buffer
	event := func() error {
		if data.Len() == 0 || data.Bytes()[0] != '{' {
			data.Reset()
			return nil
		}
		defer data.Reset()
		var e struct {
			Type string          `json:"type"`
			Item json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data.Bytes(), &e); err != nil {
			return err
		}
		if e.Type == "response.output_item.done" {
			items = append(items, append(json.RawMessage(nil), e.Item...))
		}
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(stream))
	scanner.Buffer(make([]byte, 0, 64*1024), len(stream)+1)
	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case len(line) == 0:
			if err := event(); err != nil {
				return nil, err
			}
		case bytes.HasPrefix(line, []byte("data:")):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	// A last event no blank line ended is one the stream was cut in; its item is not whole.
	return items, nil
}
