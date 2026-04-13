// Package tsnet provides a shared wrapper around tailscale.com/tsnet for
// Envoy services that need to expose endpoints on a Tailscale network with
// automatic TLS and peer identity verification.
package tsnet

import (
	"fmt"
	"os"
	"strings"
)

// Config holds tsnet configuration loaded from environment variables.
type Config struct {
	// Enabled controls whether tsnet is active. When false, services should
	// serve all routes on their legacy HTTP port.
	Enabled bool

	// Hostname is the Tailscale hostname for this node. Must be unique per
	// service per machine (e.g., "envoy-listener-sami-agents-mx").
	// Required when Enabled is true.
	Hostname string

	// StateDir is the persistent state directory for the Tailscale node.
	// Must be unique per service to avoid identity collisions.
	// Required when Enabled is true.
	StateDir string

	// AuthKey is a computed auth key derived from OAuth credentials or a
	// legacy TS_AUTHKEY. Empty when no credentials are configured (state
	// directory provides identity on restart).
	AuthKey string
}

// LoadConfig reads tsnet configuration from environment variables:
//
//   - ENVOY_TSNET_ENABLED (bool, default "false")
//   - ENVOY_TSNET_HOSTNAME (string, required when enabled)
//   - ENVOY_TSNET_STATE_DIR (string, required when enabled)
//   - ENVOY_TSNET_OAUTH_CLIENT_ID (string, optional — OAuth client ID)
//   - ENVOY_TSNET_OAUTH_CLIENT_SECRET (string, optional — OAuth client secret)
//   - ENVOY_TSNET_TAGS (string, optional — comma-separated tags, required with OAuth)
//   - ENVOY_TSNET_AUTH_KEY (string, optional — legacy auth key, mutually exclusive with OAuth)
//
// OAuth credentials take precedence over a legacy auth key. When OAuth
// credentials are provided, the client secret is used directly as the tsnet
// auth key with tag and ephemeral parameters appended (Tailscale's OAuth
// secret-as-authkey convention). Tags are required when using OAuth.
func LoadConfig() (Config, error) {
	enabled := strings.TrimSpace(os.Getenv("ENVOY_TSNET_ENABLED"))
	if enabled == "" || enabled == "false" || enabled == "0" {
		return Config{Enabled: false}, nil
	}
	if enabled != "true" && enabled != "1" {
		return Config{}, fmt.Errorf("ENVOY_TSNET_ENABLED must be 'true', 'false', '1', or '0', got %q", enabled)
	}

	hostname := strings.TrimSpace(os.Getenv("ENVOY_TSNET_HOSTNAME"))
	if hostname == "" {
		return Config{}, fmt.Errorf("ENVOY_TSNET_HOSTNAME is required when ENVOY_TSNET_ENABLED=true")
	}

	stateDir := strings.TrimSpace(os.Getenv("ENVOY_TSNET_STATE_DIR"))
	if stateDir == "" {
		return Config{}, fmt.Errorf("ENVOY_TSNET_STATE_DIR is required when ENVOY_TSNET_ENABLED=true")
	}

	authKey, err := resolveAuthKey()
	if err != nil {
		return Config{}, err
	}

	return Config{
		Enabled:  true,
		Hostname: hostname,
		StateDir: stateDir,
		AuthKey:  authKey,
	}, nil
}

// resolveAuthKey derives the tsnet auth key from environment variables.
// OAuth credentials (client secret + tags) take precedence over a legacy
// ENVOY_TSNET_AUTH_KEY. Both being set is an error to prevent confusion.
func resolveAuthKey() (string, error) {
	oauthID := strings.TrimSpace(os.Getenv("ENVOY_TSNET_OAUTH_CLIENT_ID"))
	oauthSecret := strings.TrimSpace(os.Getenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET"))
	tags := strings.TrimSpace(os.Getenv("ENVOY_TSNET_TAGS"))
	legacyKey := strings.TrimSpace(os.Getenv("ENVOY_TSNET_AUTH_KEY"))

	hasOAuth := oauthID != "" || oauthSecret != ""
	hasLegacy := legacyKey != ""

	if hasOAuth && hasLegacy {
		return "", fmt.Errorf("ENVOY_TSNET_AUTH_KEY and ENVOY_TSNET_OAUTH_CLIENT_SECRET are mutually exclusive; use one or the other")
	}

	if !hasOAuth {
		return legacyKey, nil
	}

	// OAuth path: both ID and secret are required together.
	if oauthID == "" {
		return "", fmt.Errorf("ENVOY_TSNET_OAUTH_CLIENT_ID is required when ENVOY_TSNET_OAUTH_CLIENT_SECRET is set")
	}
	if oauthSecret == "" {
		return "", fmt.Errorf("ENVOY_TSNET_OAUTH_CLIENT_SECRET is required when ENVOY_TSNET_OAUTH_CLIENT_ID is set")
	}
	if tags == "" {
		return "", fmt.Errorf("ENVOY_TSNET_TAGS is required when using OAuth credentials (e.g. 'tag:envoy')")
	}

	// Tailscale convention: an OAuth client secret can be passed directly as
	// an auth key with URL-style parameters. The node registers as non-
	// ephemeral and preauthorized, tagged with the specified tags.
	return oauthSecret + "?ephemeral=false&preauthorized=true&tags=" + tags, nil
}
