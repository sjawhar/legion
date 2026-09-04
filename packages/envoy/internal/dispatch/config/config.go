// Package config loads the shared envoy.json configuration shape used by both
// the Envoy plugin and the Dispatch server. User config (~/.config/opencode/envoy.json)
// is shallow-merged with repo config (<cwd>/.opencode/envoy.json), with repo overriding.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DispatchConfig mirrors the `dispatch` sub-object in envoy.json.
type DispatchConfig struct {
	Enabled   bool   `json:"enabled,omitempty"`
	ServerURL string `json:"serverUrl,omitempty"`
}

// EnvoyConfig is the top-level envoy.json shape.
type EnvoyConfig struct {
	Schema   string          `json:"$schema,omitempty"`
	NatsURLs []string        `json:"natsUrls,omitempty"`
	Dispatch *DispatchConfig `json:"dispatch,omitempty"`
	// Extra keys are preserved verbatim so unknown top-level fields round-trip.
	Extra map[string]json.RawMessage `json:"-"`
}

// LoadOptions controls where Load looks for config files.
type LoadOptions struct {
	CWD     string
	HomeDir string
}

var dispatchKnownKeys = map[string]struct{}{
	"enabled":   {},
	"serverUrl": {},
}

// InvalidConfigError reports an envoy.json file that failed validation — an
// unrecognized key or a malformed value. Load returns it directly instead of
// silently skipping the file: bad config is a loud failure, not a warning.
type InvalidConfigError struct {
	Path   string
	Issues []string
}

func (e *InvalidConfigError) Error() string {
	return fmt.Sprintf("invalid config %s: %s", e.Path, strings.Join(e.Issues, "; "))
}

// Load reads user and repo config and returns the shallow-merged result. A
// missing file is not an error. A file that exists but fails validation
// (an unrecognized key, a malformed value) stops the load and returns an
// *InvalidConfigError naming the file and the key.
func Load(opts LoadOptions) (*EnvoyConfig, error) {
	cwd := opts.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get cwd: %w", err)
		}
	}
	home := opts.HomeDir
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("get home: %w", err)
		}
	}

	merged := &EnvoyConfig{}
	userCfg, err := readConfigFile(filepath.Join(home, ".config", "opencode", "envoy.json"))
	if err != nil {
		return nil, err
	}
	if userCfg != nil {
		merged = mergeConfig(merged, userCfg)
	}
	repoCfg, err := readConfigFile(filepath.Join(cwd, ".opencode", "envoy.json"))
	if err != nil {
		return nil, err
	}
	if repoCfg != nil {
		merged = mergeConfig(merged, repoCfg)
	}
	return merged, nil
}

func readConfigFile(path string) (*EnvoyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	cfg, issues := parseAndValidate(data)
	if len(issues) > 0 {
		return nil, &InvalidConfigError{Path: path, Issues: issues}
	}
	return cfg, nil
}

// parseAndValidate mirrors the TS validator (envoy-client/src/dispatch-config.ts):
// type checks, unknown-key rejection under dispatch, and URL validity.
func parseAndValidate(data []byte) (*EnvoyConfig, []string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, []string{fmt.Sprintf(": %v", err)}
	}
	cfg := &EnvoyConfig{Extra: map[string]json.RawMessage{}}
	var issues []string

	if v, ok := raw["$schema"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			issues = append(issues, "$schema: Expected string")
		} else {
			cfg.Schema = s
		}
		delete(raw, "$schema")
	}
	if v, ok := raw["natsUrls"]; ok {
		var arr []string
		if err := json.Unmarshal(v, &arr); err != nil {
			issues = append(issues, "natsUrls: Expected string array")
		} else {
			cfg.NatsURLs = arr
		}
		delete(raw, "natsUrls")
	}
	if v, ok := raw["dispatch"]; ok {
		d, dIssues := parseDispatch(v)
		issues = append(issues, dIssues...)
		cfg.Dispatch = d
		delete(raw, "dispatch")
	}
	for k, v := range raw {
		cfg.Extra[k] = v
	}
	if len(issues) > 0 {
		return nil, issues
	}
	return cfg, nil
}

func parseDispatch(raw json.RawMessage) (*DispatchConfig, []string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, []string{"dispatch: Expected object"}
	}
	out := &DispatchConfig{}
	var issues []string
	for k := range m {
		if _, ok := dispatchKnownKeys[k]; !ok {
			issues = append(issues, fmt.Sprintf("dispatch.%s: Unrecognized key", k))
		}
	}
	if v, ok := m["enabled"]; ok {
		if err := json.Unmarshal(v, &out.Enabled); err != nil {
			issues = append(issues, "dispatch.enabled: Expected boolean")
		}
	}
	if v, ok := m["serverUrl"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			issues = append(issues, "dispatch.serverUrl: Expected string")
		} else if _, err := url.Parse(s); err != nil || s == "" {
			issues = append(issues, "dispatch.serverUrl: Invalid url")
		} else {
			out.ServerURL = s
		}
	}
	return out, issues
}

// mergeConfig shallow-merges base + override. The `dispatch` sub-object is also
// shallow-merged so repo dispatch keys override only matching user dispatch keys.
func mergeConfig(base, override *EnvoyConfig) *EnvoyConfig {
	out := &EnvoyConfig{
		Schema:   base.Schema,
		NatsURLs: base.NatsURLs,
		Dispatch: base.Dispatch,
		Extra:    map[string]json.RawMessage{},
	}
	for k, v := range base.Extra {
		out.Extra[k] = v
	}
	if override.Schema != "" {
		out.Schema = override.Schema
	}
	if override.NatsURLs != nil {
		out.NatsURLs = override.NatsURLs
	}
	for k, v := range override.Extra {
		out.Extra[k] = v
	}
	if base.Dispatch != nil || override.Dispatch != nil {
		merged := &DispatchConfig{}
		if base.Dispatch != nil {
			*merged = *base.Dispatch
		}
		if override.Dispatch != nil {
			if override.Dispatch.Enabled {
				merged.Enabled = true
			}
			if override.Dispatch.ServerURL != "" {
				merged.ServerURL = override.Dispatch.ServerURL
			}
		}
		out.Dispatch = merged
	}
	return out
}
