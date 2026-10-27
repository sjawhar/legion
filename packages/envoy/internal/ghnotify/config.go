package ghnotify

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The variables LoadConfig reads. Only the token turns the poller on; the cluster sets the skip
// list and the lookback, and a test sets the API base and the degraded bound.
const (
	envToken          = "ENVOY_GITHUB_NOTIFICATIONS_TOKEN"
	envSkipOwners     = "ENVOY_GITHUB_NOTIFICATIONS_SKIP_OWNERS"
	envLookback       = "ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK"
	envAPIBase        = "ENVOY_GITHUB_NOTIFICATIONS_API_BASE"
	envDegradedAfter  = "ENVOY_GITHUB_NOTIFICATIONS_DEGRADED_AFTER"
	envMentionTrigger = "ENVOY_GITHUB_MENTION_TRIGGER"
)

const (
	defaultBaseURL       = "https://api.github.com"
	defaultLookback      = time.Hour
	defaultDegradedAfter = 10 * time.Minute
	defaultTrigger       = "@legion"
	// maxLookback keeps a restart's replay inside the stream's 72-hour duplicate window, with a
	// day to spare: a longer one could republish an event the stream no longer remembers.
	maxLookback = 48 * time.Hour
	// requestTimeout bounds each request to GitHub, a client of the caller's own included.
	requestTimeout = 30 * time.Second
)

// ErrLookbackTooLong refuses a lookback past 48 hours, which could republish events older than the
// stream's 72-hour duplicate window.
var ErrLookbackTooLong = errors.New("ghnotify: the lookback is longer than 48 hours")

// Config is the poller's configuration. Every zero value takes a default in New.
type Config struct {
	// Token is the classic personal access token the poller reads GitHub as. It needs the
	// notifications scope to list threads, and repo to read a private repository's comments.
	Token string
	// SkipOwners are the GitHub owners whose threads the poller publishes nothing for: every owner
	// the Envoy GitHub App is installed on, whose webhooks already carry the same events. New
	// lower-cases them, and owners compare without regard to case, as GitHub's do.
	SkipOwners []string
	// Lookback is how far back the first poll after a start reads: 0 means 1h. New refuses one
	// past 48h with ErrLookbackTooLong.
	Lookback time.Duration
	// DegradedAfter is how long the poller may go without a successful poll before Health reports
	// it degraded: 0 means 10m.
	DegradedAfter time.Duration
	// MentionTrigger is the word whose presence in a comment or review body publishes the mention
	// copies, as the webhook route does: "" means "@legion".
	MentionTrigger string
	// BaseURL is GitHub's REST API root: "" means https://api.github.com.
	BaseURL string
	// HTTPClient sends the requests: nil means a client with a 30 s timeout. Each request is also
	// bounded to 30 s whatever client sends it.
	HTTPClient *http.Client
	// Now is the clock: nil means time.Now.
	Now func() time.Time
}

// LoadConfig reads ENVOY_GITHUB_NOTIFICATIONS_TOKEN, _SKIP_OWNERS, _LOOKBACK, _API_BASE,
// _DEGRADED_AFTER and ENVOY_GITHUB_MENTION_TRIGGER. enabled is false when the token is empty or
// blank, and then no other variable is read: a listener without the token runs no poller. A value
// it cannot use is an error naming its variable, a lookback past 48 hours one wrapping
// ErrLookbackTooLong. The cluster sets neither _API_BASE nor _DEGRADED_AFTER; the listener's tests
// do.
func LoadConfig(getenv func(string) string) (cfg Config, enabled bool, err error) {
	token := strings.TrimSpace(getenv(envToken))
	if token == "" {
		return Config{}, false, nil
	}
	cfg = Config{
		Token:          token,
		SkipOwners:     parseOwners(getenv(envSkipOwners)),
		MentionTrigger: strings.TrimSpace(getenv(envMentionTrigger)),
	}
	if cfg.Lookback, err = parseDuration(envLookback, getenv(envLookback)); err != nil {
		return Config{}, false, err
	}
	if cfg.Lookback > maxLookback {
		return Config{}, false, fmt.Errorf("%w: %s=%s", ErrLookbackTooLong, envLookback, strings.TrimSpace(getenv(envLookback)))
	}
	if cfg.DegradedAfter, err = parseDuration(envDegradedAfter, getenv(envDegradedAfter)); err != nil {
		return Config{}, false, err
	}
	if cfg.BaseURL, err = parseBaseURL(envAPIBase, getenv(envAPIBase)); err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}

// parseOwners reads a comma list of owners, trimmed and lower-cased, without blanks.
func parseOwners(raw string) []string {
	var owners []string
	for _, owner := range strings.Split(raw, ",") {
		if owner = strings.ToLower(strings.TrimSpace(owner)); owner != "" {
			owners = append(owners, owner)
		}
	}
	return owners
}

// parseDuration reads a Go duration, 0 when unset; a negative one is refused.
func parseDuration(name, raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("ghnotify: %s=%q is not a duration: %w", name, raw, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("ghnotify: %s=%q is negative", name, raw)
	}
	return d, nil
}

// parseBaseURL reads an absolute http or https URL without its trailing slash, "" when unset.
func parseBaseURL(name, raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("ghnotify: %s=%q is not an absolute http or https URL", name, raw)
	}
	return raw, nil
}
