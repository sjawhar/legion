package webhook

import "testing"

func TestLoadWebhookConfig(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantGitHub bool
		wantSlack  bool
		wantGhostW bool
		wantErr    bool
	}{
		{name: "empty ENVOY_WEBHOOKS", env: map[string]string{"ENVOY_WEBHOOKS": ""}},
		{name: "unset ENVOY_WEBHOOKS", env: map[string]string{}},
		{
			name:       "github enabled with secret",
			env:        map[string]string{"ENVOY_WEBHOOKS": "github", "ENVOY_GITHUB_WEBHOOK_SECRET": "s"},
			wantGitHub: true,
		},
		{
			name:    "github enabled without secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github"},
			wantErr: true,
		},
		{
			name:      "slack enabled with secret",
			env:       map[string]string{"ENVOY_WEBHOOKS": "slack", "ENVOY_SLACK_SIGNING_SECRET": "s"},
			wantSlack: true,
		},
		{
			name:    "slack enabled without secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "slack"},
			wantErr: true,
		},
		{
			name:       "ghostwispr enabled without secret (ok — optional)",
			env:        map[string]string{"ENVOY_WEBHOOKS": "ghostwispr"},
			wantGhostW: true,
		},
		{
			name: "ghostwispr enabled with secret",
			env: map[string]string{
				"ENVOY_WEBHOOKS":                  "ghostwispr",
				"ENVOY_GHOSTWISPR_SIGNING_SECRET": "s",
			},
			wantGhostW: true,
		},
		{
			name: "all three enabled",
			env: map[string]string{
				"ENVOY_WEBHOOKS":              "github,slack,ghostwispr",
				"ENVOY_GITHUB_WEBHOOK_SECRET": "s",
				"ENVOY_SLACK_SIGNING_SECRET":  "s",
			},
			wantGitHub: true, wantSlack: true, wantGhostW: true,
		},
		{
			name: "whitespace trimming",
			env: map[string]string{
				"ENVOY_WEBHOOKS":              " github , slack ",
				"ENVOY_GITHUB_WEBHOOK_SECRET": "s",
				"ENVOY_SLACK_SIGNING_SECRET":  "s",
			},
			wantGitHub: true, wantSlack: true,
		},
		{
			name:    "unknown provider → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "unknown"},
			wantErr: true,
		},
		{
			name:    "github with whitespace-only secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "github", "ENVOY_GITHUB_WEBHOOK_SECRET": "  "},
			wantErr: true,
		},
		{
			name:    "slack with whitespace-only secret → error",
			env:     map[string]string{"ENVOY_WEBHOOKS": "slack", "ENVOY_SLACK_SIGNING_SECRET": "  \t "},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg, err := LoadWebhookConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (cfg.GitHub != nil) != tc.wantGitHub {
				t.Errorf("GitHub enabled = %v, want %v", cfg.GitHub != nil, tc.wantGitHub)
			}
			if (cfg.Slack != nil) != tc.wantSlack {
				t.Errorf("Slack enabled = %v, want %v", cfg.Slack != nil, tc.wantSlack)
			}
			if (cfg.GhostWispr != nil) != tc.wantGhostW {
				t.Errorf("GhostWispr enabled = %v, want %v", cfg.GhostWispr != nil, tc.wantGhostW)
			}
		})
	}
}
