package webhook

import (
	"fmt"
	"os"
	"strings"
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
			cfg.GitHub = &GitHubWebhook{Secret: secret, MentionTrigger: trigger}
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
