// Package config loads the shared envoy.json configuration shape used by both
// the Envoy plugin and the Dispatch server. User config (~/.config/opencode/envoy.json)
// is shallow-merged with repo config (<cwd>/.opencode/envoy.json), with repo overriding.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
)

// DispatchConfig mirrors the `dispatch` sub-object in envoy.json.
type DispatchConfig struct {
	Enabled     bool   `json:"enabled,omitempty"`
	ServerURL   string `json:"serverUrl,omitempty"`
	DefaultRepo string `json:"defaultRepo,omitempty"`
	AppClientID string `json:"appClientId,omitempty"`
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

var (
	dispatchKnownKeys = map[string]struct{}{
		"enabled":     {},
		"serverUrl":   {},
		"defaultRepo": {},
		"appClientId": {},
	}
	repoSlugPattern = regexp.MustCompile(`^[^/]+/[^/]+$`)
)

// Load reads user and repo config and returns the shallow-merged result. Missing
// files are not errors; invalid content is logged and skipped.
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
	if userCfg, err := readConfigFile(filepath.Join(home, ".config", "opencode", "envoy.json")); err != nil {
		slog.Warn("dispatch: failed to load user config", "error", err)
	} else if userCfg != nil {
		merged = mergeConfig(merged, userCfg)
	}
	if repoCfg, err := readConfigFile(filepath.Join(cwd, ".opencode", "envoy.json")); err != nil {
		slog.Warn("dispatch: failed to load repo config", "error", err)
	} else if repoCfg != nil {
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
		slog.Warn("dispatch: invalid config", "path", path, "issues", issues)
		return nil, nil
	}
	return cfg, nil
}

// parseAndValidate runs the same validation as the original TS validator: type
// checks, unknown-key warnings under dispatch, URL validity, repo slug shape.
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
	if v, ok := m["defaultRepo"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			issues = append(issues, "dispatch.defaultRepo: Expected string")
		} else if !repoSlugPattern.MatchString(s) {
			issues = append(issues, "dispatch.defaultRepo: Invalid repository slug")
		} else {
			out.DefaultRepo = s
		}
	}
	if v, ok := m["appClientId"]; ok {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			issues = append(issues, "dispatch.appClientId: Expected string")
		} else {
			out.AppClientID = s
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
			if override.Dispatch.DefaultRepo != "" {
				merged.DefaultRepo = override.Dispatch.DefaultRepo
			}
			if override.Dispatch.AppClientID != "" {
				merged.AppClientID = override.Dispatch.AppClientID
			}
		}
		out.Dispatch = merged
	}
	return out
}
