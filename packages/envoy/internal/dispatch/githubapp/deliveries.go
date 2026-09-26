package githubapp

import (
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

// FailedDeliveries lists the App webhook's failed attempts (GitHub's status=failure: a status
// code from 400 to 599) delivered at or after since, newest first. It follows the Link header's
// cursor from page to page and stops at the first attempt older than since. GitHub keeps three
// days of attempts; nothing older is listed whatever since says.
func (c *Client) FailedDeliveries(ctx context.Context, since time.Time) ([]Delivery, error) {
	if c == nil {
		return nil, ErrNoAppKey
	}
	var failed []Delivery
	target := c.base + "/app/hook/deliveries?per_page=100&status=failure"
	for target != "" {
		jwt, err := c.appJWT()
		if err != nil {
			return nil, err
		}
		body, status, header, err := c.request(ctx, http.MethodGet, target, "Bearer "+jwt, responseLimit)
		if err != nil {
			return nil, err
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
				return failed, nil
			}
			failed = append(failed, delivery)
		}
		target = next
	}
	return failed, nil
}

// Redeliver asks GitHub to attempt a recorded delivery again
// (`POST /app/hook/deliveries/{id}/attempts`). GitHub answers 202 and delivers asynchronously;
// the outcome appears in the listing as a new attempt under the delivery's GUID.
func (c *Client) Redeliver(ctx context.Context, deliveryID int64) error {
	if c == nil {
		return ErrNoAppKey
	}
	jwt, err := c.appJWT()
	if err != nil {
		return err
	}
	target := c.base + "/app/hook/deliveries/" + strconv.FormatInt(deliveryID, 10) + "/attempts"
	body, status, err := c.do(ctx, http.MethodPost, target, "Bearer "+jwt)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("POST %s: status %d: %s", target, status, body)
	}
	return nil
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
