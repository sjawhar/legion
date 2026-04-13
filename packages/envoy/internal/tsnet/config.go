// Package tsnet provides a shared wrapper around tailscale.com/tsnet for
// Envoy services that need to expose endpoints on a Tailscale network with
// automatic TLS and peer identity verification.
package tsnet

import (
	"fmt"
	"net/url"
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

	// AuthKey is the Tailscale auth key for headless registration. Only
	// needed for initial node registration; not required on every restart
	// if state is persisted.
	//
	// This may be set directly via ENVOY_TSNET_AUTH_KEY (legacy) or
	// constructed from OAuth client credentials (preferred). See
	// resolveAuthKey for details.
	AuthKey string
}

// resolveAuthKey determines the auth key from environment variables.
// Three modes are supported (in priority order):
//
//  1. OAuth client (preferred): ENVOY_TSNET_OAUTH_CLIENT_ID +
//     ENVOY_TSNET_OAUTH_CLIENT_SECRET + ENVOY_TSNET_TAGS → constructs
//     auth key using Tailscale's OAuth-secret-as-authkey convention:
//     "secret?ephemeral=false&preauthorized=true&tags=tag:a,tag:b"
//
//  2. Legacy auth key: ENVOY_TSNET_AUTH_KEY — passed through as-is.
//
//  3. None: state directory provides identity on restart.
//
// Setting both OAuth credentials and a legacy auth key is a config error.
func resolveAuthKey() (string, error) {
	oauthID := strings.TrimSpace(os.Getenv("ENVOY_TSNET_OAUTH_CLIENT_ID"))
	oauthSecret := strings.TrimSpace(os.Getenv("ENVOY_TSNET_OAUTH_CLIENT_SECRET"))
	legacyKey := strings.TrimSpace(os.Getenv("ENVOY_TSNET_AUTH_KEY"))
	tags := strings.TrimSpace(os.Getenv("ENVOY_TSNET_TAGS"))

	hasOAuth := oauthID != "" || oauthSecret != ""
	hasLegacy := legacyKey != ""

	if hasOAuth && hasLegacy {
		return "", fmt.Errorf(
			"ENVOY_TSNET_OAUTH_CLIENT_ID/SECRET and ENVOY_TSNET_AUTH_KEY are mutually exclusive — use one or the other",
		)
	}

	if hasOAuth {
		if oauthID == "" {
			return "", fmt.Errorf("ENVOY_TSNET_OAUTH_CLIENT_ID is required when ENVOY_TSNET_OAUTH_CLIENT_SECRET is set")
		}
		if oauthSecret == "" {
			return "", fmt.Errorf("ENVOY_TSNET_OAUTH_CLIENT_SECRET is required when ENVOY_TSNET_OAUTH_CLIENT_ID is set")
		}
		if tags == "" {
			return "", fmt.Errorf("ENVOY_TSNET_TAGS is required when using OAuth client credentials (e.g. \"tag:envoy\")")
		}

		params := url.Values{}
		params.Set("ephemeral", "false")
		params.Set("preauthorized", "true")

		// Normalize tags: split, trim, rejoin without spaces.
		rawTags := strings.Split(tags, ",")
		cleaned := make([]string, 0, len(rawTags))
		for _, t := range rawTags {
			t = strings.TrimSpace(t)
			if t != "" {
				cleaned = append(cleaned, t)
			}
		}
		params.Set("tags", strings.Join(cleaned, ","))

		return oauthSecret + "?" + params.Encode(), nil
	}

	return legacyKey, nil
}

// LoadConfig reads tsnet configuration from environment variables:
//
//   - ENVOY_TSNET_ENABLED (bool, default "false")
//   - ENVOY_TSNET_HOSTNAME (string, required when enabled)
//   - ENVOY_TSNET_STATE_DIR (string, required when enabled)
//   - ENVOY_TSNET_OAUTH_CLIENT_ID (string, optional — preferred auth)
//   - ENVOY_TSNET_OAUTH_CLIENT_SECRET (string, required with client ID)
//   - ENVOY_TSNET_TAGS (string, required with OAuth — e.g. "tag:envoy")
//   - ENVOY_TSNET_AUTH_KEY (string, optional — legacy auth, mutually exclusive with OAuth)
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
