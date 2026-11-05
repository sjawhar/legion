package webhook

import (
	"fmt"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/ghrename"
)

// WebhookConfig holds parsed configuration for enabled webhook providers.
// Nil pointer = provider not enabled.
type WebhookConfig struct {
	GitHub     *GitHubWebhook
	Slack      *SlackWebhook
	GhostWispr *GhostWisprWebhook
}

// GitHubWebhook holds GitHub-specific webhook configuration.
type GitHubWebhook struct {
	Secret         string
	MentionTrigger string
	ReviewerAppID  string
	// RepositoryRenames is each repository GitHub renamed, as ENVOY_GITHUB_REPOSITORY_RENAMES
	// configures it: the listener publishes a copy of every envelope of the current repository
	// under the former name, so a session that subscribed before the rename keeps hearing about
	// it. The pair is one-way — a current name is published under its former name, never the
	// other way round — and the zero value configures none.
	RepositoryRenames ghrename.Renames
}

// SlackWebhook holds Slack-specific webhook configuration.
type SlackWebhook struct {
	Secret string
}

// GhostWisprWebhook holds Ghost Wispr-specific webhook configuration.
type GhostWisprWebhook struct {
	Secret string // empty = skip verification
}

// LoadWebhookConfig parses ENVOY_WEBHOOKS and validates required secrets.
func LoadWebhookConfig() (*WebhookConfig, error) {
	raw := os.Getenv("ENVOY_WEBHOOKS")
	if raw == "" {
		return &WebhookConfig{}, nil
	}

	cfg := &WebhookConfig{}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		switch p {
		case "github":
			secret := strings.TrimSpace(os.Getenv("ENVOY_GITHUB_WEBHOOK_SECRET"))
			if secret == "" {
				return nil, fmt.Errorf("ENVOY_GITHUB_WEBHOOK_SECRET required when github enabled")
			}
			trigger := strings.TrimSpace(os.Getenv("ENVOY_GITHUB_MENTION_TRIGGER"))
			if trigger == "" {
				trigger = "@legion"
			}
			renames, err := ghrename.Parse(os.Getenv("ENVOY_GITHUB_REPOSITORY_RENAMES"))
			if err != nil {
				return nil, fmt.Errorf("ENVOY_GITHUB_REPOSITORY_RENAMES: %w", err)
			}
			reviewerAppID := strings.TrimSpace(os.Getenv("ENVOY_REVIEWER_APP_ID"))
			if reviewerAppID == "" {
				return nil, fmt.Errorf("ENVOY_REVIEWER_APP_ID required when github enabled")
			}
			cfg.GitHub = &GitHubWebhook{
				Secret:            secret,
				MentionTrigger:    trigger,
				ReviewerAppID:     reviewerAppID,
				RepositoryRenames: renames,
			}
		case "slack":
			secret := strings.TrimSpace(os.Getenv("ENVOY_SLACK_SIGNING_SECRET"))
			if secret == "" {
				return nil, fmt.Errorf("ENVOY_SLACK_SIGNING_SECRET required when slack enabled")
			}
			cfg.Slack = &SlackWebhook{Secret: secret}
		case "ghostwispr":
			cfg.GhostWispr = &GhostWisprWebhook{Secret: strings.TrimSpace(os.Getenv("ENVOY_GHOSTWISPR_SIGNING_SECRET"))}
		default:
			return nil, fmt.Errorf("unknown webhook provider %q in ENVOY_WEBHOOKS", p)
		}
	}
	return cfg, nil
}
