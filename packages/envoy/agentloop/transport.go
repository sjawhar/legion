package agentloop

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// modelTransport carries every model call of a run. It puts the endpoint's token, read again for
// the attempt, on each request; adds the endpoint's own query parameters to it; makes the run's
// one retry, on a 429, a 5xx or a transport error, within the run's time; and, on the protocols
// that read them back, keeps each response it returns, so an OpenAI Responses turn's output
// items can be appended byte for byte as the API sent them and a Chat Completions refusal can be
// read out of the stream that carried it.
type modelTransport struct {
	base  http.RoundTripper
	kind  Kind
	token func(context.Context) (string, error)
	query url.Values

	mu sync.Mutex
	// responses are the bodies of the responses returned, one per model call, in order. They
	// are kept for the two protocols that read them back, openai-responses through
	// outputItems and openai-chat through refusal; every other kind keeps none, and reads
	// none.
	responses []*bytes.Buffer
}

// keepsResponses is whether a protocol's responses are read back out of their bodies, and so
// have to be kept as they are read.
func keepsResponses(kind Kind) bool {
	return kind == OpenAIResponses || kind == OpenAIChat
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
// the token to whatever server the Location names. A 3xx comes back as the response instead,
// which the retry leaves alone and the provider reports as its own error.
func (t *modelTransport) client() *http.Client {
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
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
		if keepsResponses(t.kind) && resp.StatusCode/100 == 2 {
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

// waitable is whether the context has the time to wait out a retry: it is not already done, and
// it either carries no deadline or has more than wait left.
func waitable(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > wait
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

// refusal is the refusal the last of the run's calls Chat Completions calls carried, or the
// empty string where it carried none.
func (t *modelTransport) refusal(calls int) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if calls <= 0 || len(t.responses) < calls {
		return "", fmt.Errorf("%d model calls finished but %d responses were kept", calls, len(t.responses))
	}
	return chatRefusal(t.responses[calls-1].Bytes()), nil
}

// chatRefusal is the refusal a Chat Completions response carries, or the empty string where it
// carries none: the choice's streamed delta.refusal, pieced together as the model wrote it, or
// the whole message.refusal a gateway answers a stream request with, read from the events of a
// stream or, for a response that is no stream at all, from the body itself. A refusal stops the
// reply for "stop" with no content, so the body is the only place it is said at all.
func chatRefusal(body []byte) string {
	var refusal strings.Builder
	read := func(data []byte) {
		var chunk struct {
			Choices []struct {
				Delta struct {
					Refusal string `json:"refusal"`
				} `json:"delta"`
				Message struct {
					Refusal string `json:"refusal"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return
		}
		for _, choice := range chunk.Choices {
			refusal.WriteString(choice.Delta.Refusal)
			refusal.WriteString(choice.Message.Refusal)
		}
	}
	streamed := false
	for data := range sseEvents(body) {
		streamed = true
		read(data)
	}
	if !streamed {
		read(body)
	}
	return refusal.String()
}

// streamItems are the items of a Responses stream's response.output_item.done events.
func streamItems(stream []byte) ([]json.RawMessage, error) {
	var items []json.RawMessage
	for data := range sseEvents(stream) {
		if data[0] != '{' {
			continue
		}
		var e struct {
			Type string          `json:"type"`
			Item json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, err
		}
		if e.Type == "response.output_item.done" {
			items = append(items, append(json.RawMessage(nil), e.Item...))
		}
	}
	return items, nil
}

// sseEvents are the data of each event of an SSE stream, in order, each event's data lines
// joined by the newlines between them; an event that carried no data line at all is left out, as
// is a last event no blank line ended, which is one the stream was cut in. The bytes an event is
// given as hold only until the next one, so whatever is read out of them is copied.
func sseEvents(stream []byte) iter.Seq[[]byte] {
	return func(yield func([]byte) bool) {
		var data bytes.Buffer
		scanner := bufio.NewScanner(bytes.NewReader(stream))
		scanner.Buffer(make([]byte, 0, 64*1024), len(stream)+1)
		for scanner.Scan() {
			line := scanner.Bytes()
			switch {
			case len(line) == 0:
				if data.Len() > 0 && !yield(data.Bytes()) {
					return
				}
				data.Reset()
			case bytes.HasPrefix(line, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
			}
		}
	}
}
