package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Delivery is one attempt GitHub recorded for the App's webhook
// (`GET /app/hook/deliveries`). A redelivery is a new attempt under the original GUID.
type Delivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Redelivery  bool      `json:"redelivery"`
	Status      string    `json:"status"`
	StatusCode  int       `json:"status_code"`
	Event       string    `json:"event"`
	// Action is empty for events that carry none (GitHub serves null).
	Action       string `json:"action"`
	RepositoryID int64  `json:"repository_id"`
}

// Deliveries lists every attempt the App webhook made at or after since, newest first, whatever
// its status: GitHub's own redelivery script needs them all, since a GUID with an OK attempt is
// delivered whatever its failures, and GitHub's status=failure filter takes only status codes from
// 400 to 599, which leaves out an attempt that got no HTTP answer. It follows the Link header's
// cursor from page to page and stops at the first attempt older than since. GitHub keeps three
// days of attempts; nothing older is listed whatever since says. A rate-limited answer is a
// *RateLimitError.
func (c *Client) Deliveries(ctx context.Context, since time.Time) ([]Delivery, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	var listed []Delivery
	target := c.base + "/app/hook/deliveries?per_page=100"
	for target != "" {
		jwt, err := c.appJWT()
		if err != nil {
			return nil, err
		}
		body, status, header, err := c.request(ctx, http.MethodGet, target, "Bearer "+jwt, responseLimit)
		if err != nil {
			return nil, err
		}
		if limited := rateLimit(status, header, body); limited != nil {
			return nil, limited
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d: %s", target, status, body)
		}
		next, err := nextLink(header.Get("Link"), c.base)
		if err != nil {
			return nil, err
		}
		var page []Delivery
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decode webhook deliveries: %w", err)
		}
		for _, delivery := range page {
			if delivery.DeliveredAt.Before(since) {
				return listed, nil
			}
			listed = append(listed, delivery)
		}
		target = next
	}
	return listed, nil
}

// Redeliver asks GitHub to attempt a recorded delivery again
// (`POST /app/hook/deliveries/{id}/attempts`). GitHub answers 202 and delivers asynchronously;
// the outcome appears in the listing as a new attempt under the delivery's GUID. A rate-limited
// answer is a *RateLimitError.
func (c *Client) Redeliver(ctx context.Context, deliveryID int64) error {
	if c == nil {
		return ErrNoAppKey
	}
	jwt, err := c.appJWT()
	if err != nil {
		return err
	}
	target := c.base + "/app/hook/deliveries/" + strconv.FormatInt(deliveryID, 10) + "/attempts"
	body, status, header, err := c.request(ctx, http.MethodPost, target, "Bearer "+jwt, responseLimit)
	if err != nil {
		return err
	}
	if limited := rateLimit(status, header, body); limited != nil {
		return limited
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("POST %s: status %d: %s", target, status, body)
	}
	return nil
}

// RateLimitError is GitHub refusing a request because the App is over a rate limit. Wait is how
// long GitHub asks for before the next request. GitHub warns that requests made while limited
// may get the integration banned.
type RateLimitError struct {
	Status int
	Wait   time.Duration
	Body   string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate-limited by GitHub (status %d), next request in %s: %s", e.Status, e.Wait, e.Body)
}

// rateLimit reads a rate-limited answer, as GitHub's REST rate-limit documentation describes one:
// a 429, or a 403 with no requests remaining, a Retry-After, or a message naming a rate limit
// (any other 403 is a refusal of that request). The wait is Retry-After's seconds, else the time
// until x-ratelimit-reset when no requests remain, else the one minute GitHub asks for when it
// gives neither.
func rateLimit(status int, header http.Header, body []byte) *RateLimitError {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return nil
	}
	retryAfter := header.Get("Retry-After")
	exhausted := header.Get("X-Ratelimit-Remaining") == "0"
	if status == http.StatusForbidden && retryAfter == "" && !exhausted && !bytes.Contains(bytes.ToLower(body), []byte("rate limit")) {
		return nil
	}
	limited := &RateLimitError{Status: status, Wait: time.Minute, Body: string(body)}
	if seconds, err := strconv.Atoi(retryAfter); err == nil {
		limited.Wait = time.Duration(seconds) * time.Second
	} else if reset, err := strconv.ParseInt(header.Get("X-Ratelimit-Reset"), 10, 64); err == nil && exhausted {
		limited.Wait = max(time.Until(time.Unix(reset, 0)), 0)
	}
	return limited
}

// nextLink returns the rel="next" target of an RFC 8288 Link header, refusing one outside the
// client's API origin: the next page is fetched with the App's JWT.
func nextLink(header, base string) (string, error) {
	for _, part := range strings.Split(header, ",") {
		segments := strings.Split(part, ";")
		if len(segments) < 2 {
			continue
		}
		isNext := false
		for _, param := range segments[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		target := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(segments[0]), "<"), ">")
		parsed, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("parse next page link %q: %w", target, err)
		}
		origin, err := url.Parse(base)
		if err != nil {
			return "", fmt.Errorf("parse API base %q: %w", base, err)
		}
		if parsed.Scheme != origin.Scheme || parsed.Host != origin.Host {
			return "", fmt.Errorf("next page link %q leaves the API origin %s", target, base)
		}
		return target, nil
	}
	return "", nil
}
