package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

// mintResponseLimit caps what a minter reads of an upstream answer. GitHub's installation pages
// and token answers and Google's token answer are a few KiB; 1 MiB is the limit Dispatch's own
// GitHub client reads them with (internal/dispatch/githubapp, responseLimit).
const mintResponseLimit = 1 << 20

// mintStatusError is an upstream answer whose status is not the one the call wanted. It wraps
// ErrMintRefused for a 4xx and ErrMintUpstream for anything else, a rate-limited 4xx among them:
// a rate limit is no verdict on the App, the login or a narrowing, and lifts by itself. It names
// the numeric status alone: an upstream's reason phrase is its own text, which a malformed or
// hostile answer can fill with what the request sent.
type mintStatusError struct {
	what, who string
	code      int
	// rateLimited is an answer that says it is a rate limit: GitHub's X-RateLimit-Remaining of 0,
	// or a body naming a rate limit, as GitHub's secondary rate limit does.
	rateLimited bool
}

func (e *mintStatusError) Error() string {
	if e.rateLimited {
		return fmt.Sprintf("%s: %s answered HTTP %d, a rate limit", e.what, e.who, e.code)
	}
	return fmt.Sprintf("%s: %s answered HTTP %d", e.what, e.who, e.code)
}

func (e *mintStatusError) Unwrap() error {
	if e.code >= 400 && e.code < 500 && !e.rateLimited {
		return ErrMintRefused
	}
	return ErrMintUpstream
}

// rateLimited reports whether response, whose body is body, says it is a rate limit, without
// quoting either.
func rateLimited(response *http.Response, body []byte) bool {
	return response.Header.Get("X-RateLimit-Remaining") == "0" || bytes.Contains(bytes.ToLower(body), []byte("rate limit"))
}

// mintCall sends request with client (http.DefaultClient when nil) and reads at most
// mintResponseLimit of its answer. what names the call and who the upstream in its errors. No error
// quotes the request, the answer or the transport's own text: Go's HTTP client quotes a malformed
// status line, and a status line or body can echo the credential the request carried, so a failed
// call is named by its class alone (transportFailure). A status other than want is a
// *mintStatusError, returned with the body, so a caller can read the upstream's own reason from it;
// every other failure wraps ErrMintUpstream.
func mintCall(client *http.Client, request *http.Request, what, who string, want int) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %s %s", ErrMintUpstream, what, who, transportFailure(request.Context(), err))
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, mintResponseLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %s %s", ErrMintUpstream, what, who, transportFailure(request.Context(), err))
	}
	if len(body) > mintResponseLimit {
		return nil, fmt.Errorf("%w: %s: %s answered more than %d bytes", ErrMintUpstream, what, who, mintResponseLimit)
	}
	if response.StatusCode != want {
		return body, &mintStatusError{what: what, who: who, code: response.StatusCode, rateLimited: rateLimited(response, body)}
	}
	return body, nil
}

// transportFailure names the class of a failed call or body read, never its text: the request was
// cancelled or ran out of time, the upstream's name did not resolve, it could not be reached, its
// TLS handshake failed, or its answer ended early. Any other failure, such as a status line that is
// not HTTP, "gave no usable HTTP answer".
func transportFailure(ctx context.Context, err error) string {
	var dns *net.DNSError
	var op *net.OpError
	var certificate *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return "was cancelled before it answered"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout():
		return "did not answer in time"
	case errors.As(err, &dns):
		return "could not be resolved"
	case errors.As(err, &certificate) || errors.As(err, &record):
		return "failed its TLS handshake"
	case errors.As(err, &op):
		return "could not be reached"
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		return "ended its answer early"
	}
	return "gave no usable HTTP answer"
}
