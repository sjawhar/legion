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

	// AuthKey is the Tailscale auth key for headless registration. Only
	// needed for initial node registration; not required on every restart
	// if state is persisted.
	AuthKey string
}

// LoadConfig reads tsnet configuration from environment variables:
//
//   - ENVOY_TSNET_ENABLED (bool, default "false")
//   - ENVOY_TSNET_HOSTNAME (string, required when enabled)
//   - ENVOY_TSNET_STATE_DIR (string, required when enabled)
//   - ENVOY_TSNET_AUTH_KEY (string, optional)
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

	authKey := strings.TrimSpace(os.Getenv("ENVOY_TSNET_AUTH_KEY"))

	return Config{
		Enabled:  true,
		Hostname: hostname,
		StateDir: stateDir,
		AuthKey:  authKey,
	}, nil
}
