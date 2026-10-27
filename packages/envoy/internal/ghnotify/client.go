package ghnotify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// client reads GitHub. Its one method is get: the poller never changes anything on GitHub, so it
// never marks a notification read or changes a subscription (R5).
type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// get sends one GET for path, a path under the API root or an absolute URL GitHub handed back in a
// Link header, with query and header added. An absolute URL must be on the API root's scheme and
// host, so the token never leaves for another server. The request is bounded to requestTimeout;
// the bound ends when the caller closes the body.
func (c *client) get(ctx context.Context, path string, query url.Values, header http.Header) (*http.Response, error) {
	target, err := resolveURL(c.base, path, query)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "envoy-github-notifications")
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func resolveURL(base *url.URL, path string, query url.Values) (string, error) {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		u, err := url.Parse(path)
		if err != nil {
			return "", err
		}
		if u.Scheme != base.Scheme || u.Host != base.Host {
			return "", fmt.Errorf("ghnotify: GitHub linked to %s, outside %s", u.Host, base.Host)
		}
		if len(query) > 0 {
			merged := u.Query()
			for name, values := range query {
				merged[name] = values
			}
			u.RawQuery = merged.Encode()
		}
		return u.String(), nil
	}
	target := base.String() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target, nil
}

// failure classes a request GitHub did not answer with what the poller asked for. Each class has
// its own consequence for the thread and the cursor (the spec's "One thread").
type failure int

const (
	// failTransient is a 5xx, a timeout, a lost connection or a body that would not decode: the
	// poller keeps the old thread state and the old cursor, and the next poll tries again.
	failTransient failure = iota + 1
	// failUnauthorized is a 401: the token is refused.
	failUnauthorized
	// failRateLimited is a 403 or 429 carrying Retry-After or X-RateLimit-Remaining 0: the poller
	// waits as GitHub says and keeps the old state.
	failRateLimited
	// failSSO is a 403 with X-GitHub-SSO: the owner requires single sign-on for the token.
	failSSO
	// failUnreadable is any other 403, a 404 or another 4xx: the poller cannot read the thread.
	failUnreadable
	// failFloor is a detail fetch the rate floor held back, never sent.
	failFloor
)

type fetchError struct {
	kind   failure
	status int
	wait   time.Duration
	err    error
}

func (e *fetchError) Error() string {
	switch {
	case e.err != nil:
		return e.err.Error()
	case e.kind == failFloor:
		return "ghnotify: below the rate floor"
	default:
		return fmt.Sprintf("ghnotify: GitHub answered %d", e.status)
	}
}

func (e *fetchError) Unwrap() error { return e.err }

// failureOf is err's class, transient for anything that is not a fetchError.
func failureOf(err error) *fetchError {
	var fe *fetchError
	if errors.As(err, &fe) {
		return fe
	}
	return &fetchError{kind: failTransient, err: err}
}

// classify is the failure of an answer that is neither 200 nor 304.
func classify(resp *http.Response, now time.Time) *fetchError {
	fe := &fetchError{status: resp.StatusCode}
	switch status := resp.StatusCode; {
	case status == http.StatusUnauthorized:
		fe.kind = failUnauthorized
	case status == http.StatusForbidden && resp.Header.Get("X-GitHub-SSO") != "":
		fe.kind = failSSO
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		if wait, ok := rateLimitWait(resp.Header, now); ok {
			fe.kind, fe.wait = failRateLimited, wait
		} else if status == http.StatusTooManyRequests {
			fe.kind, fe.wait = failRateLimited, minInterval
		} else {
			fe.kind = failUnreadable
		}
	case status >= 500:
		fe.kind = failTransient
	case status >= 400:
		fe.kind = failUnreadable
	default:
		fe.kind = failTransient
	}
	return fe
}

// rateLimitWait is how long a rate-limited answer tells the poller to wait: Retry-After, in seconds
// or as a date, else the time to X-RateLimit-Reset when X-RateLimit-Remaining is 0.
func rateLimitWait(h http.Header, now time.Time) (time.Duration, bool) {
	if raw := strings.TrimSpace(h.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second, true
		}
		if at, err := http.ParseTime(raw); err == nil {
			return max(at.Sub(now), 0), true
		}
	}
	if strings.TrimSpace(h.Get("X-RateLimit-Remaining")) == "0" {
		if reset, ok := rateLimitReset(h); ok {
			return max(reset.Sub(now), 0), true
		}
		return minInterval, true
	}
	return 0, false
}

func rateLimitReset(h http.Header) (time.Time, bool) {
	seconds, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0), true
}

// parseLinks reads a Link header into its targets by rel.
func parseLinks(header string) map[string]string {
	links := map[string]string{}
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		target := strings.TrimSpace(fields[0])
		if len(fields) < 2 || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		for _, param := range fields[1:] {
			rel, ok := strings.CutPrefix(strings.TrimSpace(param), "rel=")
			if !ok {
				continue
			}
			for _, name := range strings.Fields(strings.Trim(rel, `"`)) {
				links[name] = target[1 : len(target)-1]
			}
		}
	}
	return links
}

// pageOf is the page number a Link target names, 0 when it names none.
func pageOf(target string) int {
	u, err := url.Parse(target)
	if err != nil {
		return 0
	}
	page, _ := strconv.Atoi(u.Query().Get("page"))
	return page
}

// withPage is target naming page instead.
func withPage(target string, page int) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	query := u.Query()
	query.Set("page", strconv.Itoa(page))
	u.RawQuery = query.Encode()
	return u.String()
}
