// Envoy GitHub App credentials, loaded from disk at startup.
//
// The App itself is created once at github.com/settings/apps/new (see
// packages/envoy/cmd/dispatch/README.md for the setup checklist). The
// resulting client_id, client_secret, webhook secret, and private key (PEM)
// are written by hand into ~/.local/share/dispatch/app.json — dispatch only
// reads this file, never creates or modifies it.
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// AppConfig is the persisted Envoy App credentials.
type AppConfig struct {
	ID            int64    `json:"id,omitempty"`
	Slug          string   `json:"slug,omitempty"`
	Name          string   `json:"name,omitempty"`
	HTMLURL       string   `json:"htmlUrl,omitempty"`
	ClientID      string   `json:"clientId"`
	ClientSecret  string   `json:"clientSecret"`
	WebhookSecret string   `json:"webhookSecret,omitempty"`
	PEM           string   `json:"pem,omitempty"`
	OwnerLogin    string   `json:"ownerLogin,omitempty"`
	Permissions   AppPerms `json:"permissions,omitempty"`
}

// AppPerms mirrors the GitHub Apps permissions object. We surface only the
// fields dispatch actually inspects; unknown perms round-trip via the JSON
// blob if callers re-serialize.
type AppPerms struct {
	Issues       string `json:"issues,omitempty"`
	PullRequests string `json:"pullRequests,omitempty"`
	Contents     string `json:"contents,omitempty"`
	Metadata     string `json:"metadata,omitempty"`
}

// DefaultAppPath returns ~/.local/share/dispatch/app.json.
func DefaultAppPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home: %w", err)
	}
	return filepath.Join(home, ".local", "share", "dispatch", "app.json"), nil
}

// ReadApp returns (nil, nil) when the file does not exist so callers can
// degrade gracefully (e.g. respond 503 with a "configure your Envoy App"
// hint).
func ReadApp(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var cfg AppConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("%s missing required fields (clientId, clientSecret)", path)
	}
	return &cfg, nil
}

// WriteApp is a convenience for tests; production app.json is hand-edited.
func WriteApp(path string, cfg *AppConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadAppFromEnv assembles an AppConfig from environment variables. Used
// by production deployments where credentials come from a secrets manager
// via the container task definition; the filesystem app.json is the dev
// path.
//
// Required env vars:
//
//	DISPATCH_APP_CLIENT_ID
//	DISPATCH_APP_CLIENT_SECRET
//	DISPATCH_APP_PEM_B64        (base64-encoded PEM — multiline PEM is
//	                              awkward to ship through most container
//	                              env interfaces, so we accept base64)
//
// Optional:
//
//	DISPATCH_APP_ID                (integer)
//	DISPATCH_APP_SLUG
//	DISPATCH_APP_NAME
//	DISPATCH_APP_WEBHOOK_SECRET
//
// Returns (nil, nil) when DISPATCH_APP_CLIENT_ID is unset so callers can
// fall through to the file-based path.
func LoadAppFromEnv() (*AppConfig, error) {
	clientID := os.Getenv("DISPATCH_APP_CLIENT_ID")
	if clientID == "" {
		return nil, nil
	}
	clientSecret := os.Getenv("DISPATCH_APP_CLIENT_SECRET")
	if clientSecret == "" {
		return nil, fmt.Errorf("DISPATCH_APP_CLIENT_ID set but DISPATCH_APP_CLIENT_SECRET missing")
	}
	pem := ""
	if b64 := os.Getenv("DISPATCH_APP_PEM_B64"); b64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("DISPATCH_APP_PEM_B64: %w", err)
		}
		pem = string(decoded)
	}
	var appID int64
	if raw := os.Getenv("DISPATCH_APP_ID"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("DISPATCH_APP_ID: %w", err)
		}
		appID = n
	}
	return &AppConfig{
		ID:            appID,
		Slug:          os.Getenv("DISPATCH_APP_SLUG"),
		Name:          os.Getenv("DISPATCH_APP_NAME"),
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		WebhookSecret: os.Getenv("DISPATCH_APP_WEBHOOK_SECRET"),
		PEM:           pem,
	}, nil
}
