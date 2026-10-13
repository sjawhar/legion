// Package config loads envoy.json, the configuration file the Dispatch and Envoy clients and the
// Dispatch server share. The user file, ${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json, is
// shallow-merged with the checkout's file, <cwd>/.envoy/envoy.json, the checkout's file winning.
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

// DispatchConfig mirrors the `dispatch` sub-object in envoy.json. Token is the value as written:
// one starting with `!` is a shell command only the clients run, and the server reads no token.
type DispatchConfig struct {
	Enabled   bool   `json:"enabled,omitempty"`
	ServerURL string `json:"serverUrl,omitempty"`
	Token     string `json:"token,omitempty"`
}

// EnvoySection is what the server reads of the `envoy` sub-object in envoy.json: its NATS URLs.
// parseEnvoy checks every key the section allows (enabled, url, natsUrls, token, natsSeed), and
// keeps natsUrls alone.
type EnvoySection struct {
	NatsURLs []string `json:"natsUrls,omitempty"`
}

// EnvoyConfig is the top-level envoy.json shape.
type EnvoyConfig struct {
	Schema   string          `json:"$schema,omitempty"`
	Dispatch *DispatchConfig `json:"dispatch,omitempty"`
	Envoy    EnvoySection    `json:"envoy"`
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
	"token":     {},
}

var envoyKnownKeys = map[string]struct{}{
	"enabled":  {},
	"url":      {},
	"natsUrls": {},
	"token":    {},
	"natsSeed": {},
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

// Load reads the user and checkout files and applies the DISPATCH_SERVER_URL and NATS_URLS
// overrides from environment, which answers as os.LookupEnv does: a variable that is set overrides
// the files, and one set but empty is refused. environment also names XDG_CONFIG_HOME, the
// directory holding the user file's legion directory; unset or empty, it is ~/.config.
// envoy-dispatch hands Load its settings table's lookup; any other caller names the environment
// it means. A missing file is not an error, except that a user file still at its OpenCode-era
// path, ~/.config/opencode/envoy.json, with none at the current one is refused, naming both. A
// file that exists but fails validation (an unrecognized key, a malformed value) stops the load
// and returns an *InvalidConfigError naming the file and the key.
func Load(environment func(string) (string, bool), opts LoadOptions) (*EnvoyConfig, error) {
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
	configHome, _ := environment("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}

	merged := &EnvoyConfig{}
	userPath := filepath.Join(configHome, "legion", "envoy.json")
	userCfg, err := readConfigFile(userPath)
	if err != nil {
		return nil, err
	}
	if userCfg == nil {
		if err := refuseOpenCodeUserFile(filepath.Join(home, ".config", "opencode", "envoy.json"), userPath); err != nil {
			return nil, err
		}
	} else {
		merged = mergeConfig(merged, userCfg)
	}
	checkoutCfg, err := readConfigFile(filepath.Join(cwd, ".envoy", "envoy.json"))
	if err != nil {
		return nil, err
	}
	if checkoutCfg != nil {
		merged = mergeConfig(merged, checkoutCfg)
	}
	if err := applyEnvironmentOverrides(merged, environment); err != nil {
		return nil, err
	}
	return merged, nil
}

// refuseOpenCodeUserFile refuses a user file still at oldPath, which nothing reads, while none is
// at userPath.
func refuseOpenCodeUserFile(oldPath, userPath string) error {
	_, err := os.Stat(oldPath)
	switch {
	case err == nil:
		return fmt.Errorf("%s is no longer read: move it to %s", oldPath, userPath)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("check for the no longer read %s: %w", oldPath, err)
	}
}

func applyEnvironmentOverrides(cfg *EnvoyConfig, lookup func(string) (string, bool)) error {
	if raw, set := lookup("DISPATCH_SERVER_URL"); set {
		serverURL, err := dispatchServerURL(raw)
		if err != nil {
			return fmt.Errorf("DISPATCH_SERVER_URL=%q (expected an absolute http(s) URL without a path)", raw)
		}
		if cfg.Dispatch == nil {
			cfg.Dispatch = &DispatchConfig{}
		}
		cfg.Dispatch.ServerURL = serverURL
	}
	if raw, set := lookup("NATS_URLS"); set {
		natsURLs, err := natsURLs(raw)
		if err != nil {
			return fmt.Errorf("NATS_URLS=%q (expected a comma-separated list of NATS URLs)", raw)
		}
		cfg.Envoy.NatsURLs = natsURLs
	}
	return nil
}

func dispatchServerURL(raw string) (string, error) {
	serverURL := strings.TrimSpace(raw)
	parsed, err := url.Parse(serverURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Hostname() == "" || parsed.Path != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("invalid dispatch server URL")
	}
	return serverURL, nil
}

func natsURLs(raw string) ([]string, error) {
	values := strings.Split(raw, ",")
	for i, value := range values {
		values[i] = strings.TrimSpace(value)
		if values[i] == "" {
			return nil, errors.New("empty NATS URL")
		}
	}
	return values, nil
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
// type checks, unknown-key rejection under dispatch and envoy, URL validity, and the refusal of
// the top-level natsUrls, which is envoy.natsUrls.
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
	if _, ok := raw["natsUrls"]; ok {
		issues = append(issues, "natsUrls: no longer read; write it as envoy.natsUrls")
		delete(raw, "natsUrls")
	}
	if v, ok := raw["dispatch"]; ok {
		d, dIssues := parseDispatch(v)
		issues = append(issues, dIssues...)
		cfg.Dispatch = d
		delete(raw, "dispatch")
	}
	if v, ok := raw["envoy"]; ok {
		e, eIssues := parseEnvoy(v)
		issues = append(issues, eIssues...)
		cfg.Envoy = e
		delete(raw, "envoy")
	}
	for k, v := range raw {
		cfg.Extra[k] = v
	}
	if len(issues) > 0 {
		return nil, issues
	}
	return cfg, nil
}

// unknownKeys names each key of m that known does not list, under section.
func unknownKeys(section string, m map[string]json.RawMessage, known map[string]struct{}) []string {
	var issues []string
	for k := range m {
		if _, ok := known[k]; !ok {
			issues = append(issues, fmt.Sprintf("%s.%s: Unrecognized key", section, k))
		}
	}
	return issues
}

// parseURL reads a section's URL value, as the TS validator's url() check reads it.
func parseURL(field string, raw json.RawMessage) (string, []string) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", []string{field + ": Expected string"}
	}
	if _, err := url.Parse(s); err != nil || s == "" {
		return "", []string{field + ": Invalid url"}
	}
	return s, nil
}

func parseDispatch(raw json.RawMessage) (*DispatchConfig, []string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, []string{"dispatch: Expected object"}
	}
	out := &DispatchConfig{}
	issues := unknownKeys("dispatch", m, dispatchKnownKeys)
	if v, ok := m["enabled"]; ok {
		if err := json.Unmarshal(v, &out.Enabled); err != nil {
			issues = append(issues, "dispatch.enabled: Expected boolean")
		}
	}
	if v, ok := m["serverUrl"]; ok {
		serverURL, urlIssues := parseURL("dispatch.serverUrl", v)
		issues = append(issues, urlIssues...)
		out.ServerURL = serverURL
	}
	if v, ok := m["token"]; ok {
		if err := json.Unmarshal(v, &out.Token); err != nil {
			issues = append(issues, "dispatch.token: Expected string")
		}
	}
	return out, issues
}

func parseEnvoy(raw json.RawMessage) (EnvoySection, []string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return EnvoySection{}, []string{"envoy: Expected object"}
	}
	var out EnvoySection
	issues := unknownKeys("envoy", m, envoyKnownKeys)
	if v, ok := m["enabled"]; ok {
		var enabled bool
		if err := json.Unmarshal(v, &enabled); err != nil {
			issues = append(issues, "envoy.enabled: Expected boolean")
		}
	}
	if v, ok := m["url"]; ok {
		_, urlIssues := parseURL("envoy.url", v)
		issues = append(issues, urlIssues...)
	}
	if v, ok := m["natsUrls"]; ok {
		if err := json.Unmarshal(v, &out.NatsURLs); err != nil {
			issues = append(issues, "envoy.natsUrls: Expected string array")
		}
	}
	for _, key := range []string{"token", "natsSeed"} {
		if v, ok := m[key]; ok {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				issues = append(issues, "envoy."+key+": Expected string")
			}
		}
	}
	return out, issues
}

// mergeConfig shallow-merges base + override. The `dispatch` and `envoy` sub-objects are also
// shallow-merged so checkout keys override only matching user keys.
func mergeConfig(base, override *EnvoyConfig) *EnvoyConfig {
	out := &EnvoyConfig{
		Schema:   base.Schema,
		Dispatch: base.Dispatch,
		Envoy:    base.Envoy,
		Extra:    map[string]json.RawMessage{},
	}
	for k, v := range base.Extra {
		out.Extra[k] = v
	}
	if override.Schema != "" {
		out.Schema = override.Schema
	}
	if override.Envoy.NatsURLs != nil {
		out.Envoy.NatsURLs = override.Envoy.NatsURLs
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
			if override.Dispatch.Token != "" {
				merged.Token = override.Dispatch.Token
			}
		}
		out.Dispatch = merged
	}
	return out
}
