package secrets

import (
	"fmt"
	"io"
	"net/http"
)

// mintResponseLimit caps what a minter reads of an upstream answer. GitHub's installation pages
// and token answers and Google's token answer are a few KiB; 1 MiB is the limit Dispatch's own
// GitHub client reads them with (internal/dispatch/githubapp, responseLimit).
const mintResponseLimit = 1 << 20

// mintStatusError is an upstream answer whose status is not the one the call wanted. It wraps
// ErrMintRefused for a 4xx and ErrMintUpstream for anything else.
type mintStatusError struct {
	what, who, status string
	code              int
}

func (e *mintStatusError) Error() string {
	return fmt.Sprintf("%s: %s returned %s", e.what, e.who, e.status)
}

func (e *mintStatusError) Unwrap() error {
	if e.code >= 400 && e.code < 500 {
		return ErrMintRefused
	}
	return ErrMintUpstream
}

// mintCall sends request with client (http.DefaultClient when nil) and reads at most
// mintResponseLimit of its answer. what names the call and who the upstream in its errors, none
// of which quotes the request or the answer. A status other than want is a *mintStatusError,
// returned with the body, so a caller can read the upstream's own reason; every other failure
// wraps ErrMintUpstream.
func mintCall(client *http.Client, request *http.Request, what, who string, want int) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrMintUpstream, what, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, mintResponseLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrMintUpstream, what, err)
	}
	if len(body) > mintResponseLimit {
		return nil, fmt.Errorf("%w: %s: %s answered more than %d bytes", ErrMintUpstream, what, who, mintResponseLimit)
	}
	if response.StatusCode != want {
		return body, &mintStatusError{what: what, who: who, status: response.Status, code: response.StatusCode}
	}
	return body, nil
}
