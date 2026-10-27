package ghnotify

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

func envFrom(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestLoadConfig(t *testing.T) {
	t.Run("an empty token is a listener without the poller", func(t *testing.T) {
		for _, token := range []string{"", "  \t"} {
			cfg, enabled, err := LoadConfig(envFrom(map[string]string{
				"ENVOY_GITHUB_NOTIFICATIONS_TOKEN": token,
				// Nothing else is read without a token, so a bad value beside it refuses nothing.
				"ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK": "49h",
			}))
			if err != nil || enabled {
				t.Fatalf("token %q: enabled %v err %v, want disabled and no error", token, enabled, err)
			}
			if cfg.Token != "" {
				t.Fatalf("token %q: config carries a token %q", token, cfg.Token)
			}
		}
	})

	t.Run("a token alone leaves every other setting to New's defaults", func(t *testing.T) {
		cfg, enabled, err := LoadConfig(envFrom(map[string]string{"ENVOY_GITHUB_NOTIFICATIONS_TOKEN": " ghp_example "}))
		if err != nil || !enabled {
			t.Fatalf("enabled %v err %v", enabled, err)
		}
		if cfg.Token != "ghp_example" {
			t.Fatalf("token %q, want it trimmed", cfg.Token)
		}
		if cfg.Lookback != 0 || cfg.DegradedAfter != 0 || cfg.BaseURL != "" || cfg.MentionTrigger != "" || len(cfg.SkipOwners) != 0 {
			t.Fatalf("unset variables gave %+v", cfg)
		}
	})

	t.Run("every variable is read", func(t *testing.T) {
		cfg, enabled, err := LoadConfig(envFrom(map[string]string{
			"ENVOY_GITHUB_NOTIFICATIONS_TOKEN":          "ghp_example",
			"ENVOY_GITHUB_NOTIFICATIONS_SKIP_OWNERS":    " Acme , ,Covered-Org,",
			"ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK":       "48h",
			"ENVOY_GITHUB_NOTIFICATIONS_API_BASE":       "http://127.0.0.1:8080/",
			"ENVOY_GITHUB_NOTIFICATIONS_DEGRADED_AFTER": "2s",
			"ENVOY_GITHUB_MENTION_TRIGGER":              " @example-bot ",
		}))
		if err != nil || !enabled {
			t.Fatalf("enabled %v err %v", enabled, err)
		}
		if strings.Join(cfg.SkipOwners, ",") != "acme,covered-org" {
			t.Fatalf("skip owners %q, want trimmed, lower-cased and without blanks", cfg.SkipOwners)
		}
		if cfg.Lookback != 48*time.Hour {
			t.Fatalf("lookback %s", cfg.Lookback)
		}
		if cfg.BaseURL != "http://127.0.0.1:8080" {
			t.Fatalf("base URL %q, want its trailing slash dropped", cfg.BaseURL)
		}
		if cfg.DegradedAfter != 2*time.Second {
			t.Fatalf("degraded after %s", cfg.DegradedAfter)
		}
		if cfg.MentionTrigger != "@example-bot" {
			t.Fatalf("mention trigger %q", cfg.MentionTrigger)
		}
	})

	t.Run("a lookback past 48 hours is refused, naming its value", func(t *testing.T) {
		_, _, err := LoadConfig(envFrom(map[string]string{
			"ENVOY_GITHUB_NOTIFICATIONS_TOKEN":    "ghp_example",
			"ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK": "49h",
		}))
		if !errors.Is(err, ErrLookbackTooLong) {
			t.Fatalf("err %v, want ErrLookbackTooLong", err)
		}
		if !strings.Contains(err.Error(), "ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK") || !strings.Contains(err.Error(), "49h") {
			t.Fatalf("err %q names neither the variable nor its value", err)
		}
	})

	for _, tc := range []struct{ name, variable, value string }{
		{"a lookback that is not a duration", "ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK", "soon"},
		{"a negative lookback", "ENVOY_GITHUB_NOTIFICATIONS_LOOKBACK", "-1h"},
		{"a degraded bound that is not a duration", "ENVOY_GITHUB_NOTIFICATIONS_DEGRADED_AFTER", "later"},
		{"a negative degraded bound", "ENVOY_GITHUB_NOTIFICATIONS_DEGRADED_AFTER", "-2s"},
		{"an API base that is not an absolute URL", "ENVOY_GITHUB_NOTIFICATIONS_API_BASE", "api.github.com"},
		{"an API base that is not http", "ENVOY_GITHUB_NOTIFICATIONS_API_BASE", "ftp://api.example"},
	} {
		t.Run(tc.name+" is refused, naming the variable", func(t *testing.T) {
			_, _, err := LoadConfig(envFrom(map[string]string{
				"ENVOY_GITHUB_NOTIFICATIONS_TOKEN": "ghp_example",
				tc.variable:                        tc.value,
			}))
			if err == nil || !strings.Contains(err.Error(), tc.variable) {
				t.Fatalf("err %v, want a refusal naming %s", err, tc.variable)
			}
		})
	}
}

func TestNew(t *testing.T) {
	logger := logging.NewWithWriter("test", &syncBuffer{})
	pub := newFakePublisher()

	t.Run("a lookback past 48 hours is refused", func(t *testing.T) {
		_, err := New(Config{Token: "ghp_example", Lookback: 49 * time.Hour}, pub, logger, metrics.New())
		if !errors.Is(err, ErrLookbackTooLong) {
			t.Fatalf("err %v, want ErrLookbackTooLong", err)
		}
	})

	t.Run("no token, publisher or logger is refused", func(t *testing.T) {
		if _, err := New(Config{}, pub, logger, metrics.New()); err == nil {
			t.Fatal("New took an empty token")
		}
		if _, err := New(Config{Token: "ghp_example"}, nil, logger, metrics.New()); err == nil {
			t.Fatal("New took no publisher")
		}
		if _, err := New(Config{Token: "ghp_example"}, pub, nil, metrics.New()); err == nil {
			t.Fatal("New took no logger")
		}
	})

	t.Run("zero values take the defaults", func(t *testing.T) {
		p, err := New(Config{Token: "ghp_example", SkipOwners: []string{"Acme"}}, pub, logger, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if p.cfg.Lookback != time.Hour || p.cfg.DegradedAfter != 10*time.Minute || p.cfg.MentionTrigger != "@legion" {
			t.Fatalf("defaults: lookback %s degraded %s trigger %q", p.cfg.Lookback, p.cfg.DegradedAfter, p.cfg.MentionTrigger)
		}
		if p.cfg.BaseURL != "https://api.github.com" || p.cfg.HTTPClient == nil || p.cfg.HTTPClient.Timeout != 30*time.Second || p.cfg.Now == nil {
			t.Fatalf("defaults: base %q client %v", p.cfg.BaseURL, p.cfg.HTTPClient)
		}
		if _, ok := p.skip["acme"]; !ok {
			t.Fatalf("skip owners %v, want them lower-cased", p.skip)
		}
		if p.cursor().since.IsZero() {
			t.Fatal("the cursor starts with no since")
		}
	})
}
