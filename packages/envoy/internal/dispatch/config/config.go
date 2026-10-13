// Package config loads envoy.json, the configuration file the Dispatch and Envoy clients and the
// Dispatch server share. The user file, ${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json, is
// shallow-merged with the checkout's file, <cwd>/.envoy/envoy.json, the checkout's dispatch keys
// winning. A checkout's file may hold no envoy section: a cloned repository does not choose the
// NATS servers a process connects to.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// DispatchConfig is what the server reads of the `dispatch` sub-object in envoy.json: its URL.
// parseDispatch checks every key the section allows (enabled, serverUrl, token); `enabled` and
// `token` are a session's, which the clients resolve (packages/envoy-client), and the server reads
// neither.
type DispatchConfig struct {
	ServerURL string
}

// EnvoySection is what the server reads of the `envoy` sub-object in envoy.json: its NATS URLs,
// whatever `enabled` says, since `enabled` switches a session's Envoy and not the server's.
// parseEnvoy checks every key the section allows (enabled, url, natsUrls, token, natsSeed), and
// keeps natsUrls alone.
type EnvoySection struct {
	NatsURLs []string
}

// EnvoyConfig is what the server reads of envoy.json. Other top-level keys are allowed and not
// read.
type EnvoyConfig struct {
	Dispatch *DispatchConfig
	Envoy    EnvoySection
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
// directory holding the user file's legion directory: unset or empty, it is ~/.config, and a value
// that is not an absolute path is refused. The XDG Base Directory spec says to ignore such a value;
// os.UserConfigDir refuses it, and so does Load, rather than read a file found from wherever the
// process started or fall back silently. envoy-dispatch hands Load its settings table's lookup; any
// other caller names the environment it means. A missing file is not an error, except that while
// the user file is ~/.config/legion/envoy.json (XDG_CONFIG_HOME unset, empty or ~/.config) and
// none is there, a user file still at its OpenCode-era path beside it,
// ~/.config/opencode/envoy.json, is refused, naming both. A file that exists but fails validation
// (an unrecognized key, a malformed value, an envoy section in the checkout's file) stops the load
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
	switch {
	case configHome == "":
		configHome = filepath.Join(home, ".config")
	case !filepath.IsAbs(configHome):
		return nil, fmt.Errorf("XDG_CONFIG_HOME is %q, which is not an absolute path", configHome)
	}

	merged := &EnvoyConfig{}
	userPath := filepath.Join(configHome, "legion", "envoy.json")
	userCfg, err := readConfigFile(userPath, "")
	if err != nil {
		return nil, err
	}
	switch {
	case userCfg != nil:
		merged = userCfg
	case userPath == filepath.Join(home, ".config", "legion", "envoy.json"):
		// The old file is the user's only beside its own directory: an XDG_CONFIG_HOME naming another
		// directory is not where it belongs.
		if err := refuseOpenCodeUserFile(filepath.Join(home, ".config", "opencode", "envoy.json"), userPath); err != nil {
			return nil, err
		}
	}
	checkoutCfg, err := readConfigFile(filepath.Join(cwd, ".envoy", "envoy.json"), userPath)
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

// readConfigFile reads one envoy.json, or nil when there is none: a missing file, or a path through
// a regular file (a repository with a file named .envoy). checkoutOf is empty for the user file;
// for a checkout's file it is the user file's path, where the envoy section a checkout's file may
// not hold belongs.
func readConfigFile(path, checkoutOf string) (*EnvoyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		return nil, err
	}
	cfg, issues := parseAndValidate(data, checkoutOf)
	if len(issues) > 0 {
		return nil, &InvalidConfigError{Path: path, Issues: issues}
	}
	return cfg, nil
}

// isNull reports a JSON null, which every key of envoy.json refuses, as the TS schema does.
func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// jsonError describes why data is not a JSON object without quoting it: a syntax error by its line
// and column, since the decoder's message quotes the character it stopped at, which can be the
// start of an unquoted token or seed.
func jsonError(data []byte, err error) string {
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		return ": Expected object"
	}
	// Offset counts the byte the decoder stopped at; the position named is that byte's.
	before := data[:max(0, min(int(syntax.Offset)-1, len(data)))]
	line := bytes.Count(before, []byte("\n")) + 1
	column := len(before) - bytes.LastIndexByte(before, '\n')
	return fmt.Sprintf(": invalid JSON at line %d, column %d", line, column)
}

// parseAndValidate mirrors the TS validator (envoy-client/src/envoy-config.ts): an object at the
// top, type checks with null refused, unknown-key rejection under dispatch and envoy, http(s) URLs,
// non-empty NATS URLs, the refusal of the top-level natsUrls, which is envoy.natsUrls, and, for a
// checkout's file (checkoutOf set), the refusal of any envoy section, naming the user file it
// belongs in.
func parseAndValidate(data []byte, checkoutOf string) (*EnvoyConfig, []string) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, []string{jsonError(data, err)}
	}
	if raw == nil {
		return nil, []string{": Expected object"}
	}
	cfg := &EnvoyConfig{}
	var issues []string

	if v, ok := raw["$schema"]; ok {
		var s string
		issues = append(issues, checkType("$schema", "string", v, &s)...)
	}
	if _, ok := raw["natsUrls"]; ok {
		if checkoutOf != "" {
			issues = append(issues, "natsUrls: not read, and a checkout's envoy.json holds only a dispatch section")
		} else {
			issues = append(issues, "natsUrls: no longer read; write it as envoy.natsUrls")
		}
	}
	if v, ok := raw["dispatch"]; ok {
		d, dIssues := parseDispatch(v)
		issues = append(issues, dIssues...)
		cfg.Dispatch = d
	}
	if v, ok := raw["envoy"]; ok {
		if checkoutOf != "" {
			issues = append(issues, "envoy: not read from a checkout's envoy.json; put it in "+checkoutOf)
		} else {
			e, eIssues := parseEnvoy(v)
			issues = append(issues, eIssues...)
			cfg.Envoy = e
		}
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

// section reads one section's object, refusing anything else (null included).
func section(name string, raw json.RawMessage) (map[string]json.RawMessage, []string) {
	var m map[string]json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &m) != nil {
		return nil, []string{name + ": Expected object"}
	}
	return m, nil
}

// checkType refuses a value that is null or does not decode into into.
func checkType(field, want string, raw json.RawMessage, into any) []string {
	if isNull(raw) || json.Unmarshal(raw, into) != nil {
		return []string{field + ": Expected " + want}
	}
	return nil
}

// parseURL reads a section's URL value, as the TS validator reads it: an absolute http or https
// URL with a host.
func parseURL(field string, raw json.RawMessage) (string, []string) {
	var s string
	if issues := checkType(field, "string", raw, &s); issues != nil {
		return "", issues
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", []string{field + ": must be an http or https URL"}
	}
	return s, nil
}

func parseDispatch(raw json.RawMessage) (*DispatchConfig, []string) {
	m, issues := section("dispatch", raw)
	if issues != nil {
		return nil, issues
	}
	out := &DispatchConfig{}
	issues = unknownKeys("dispatch", m, dispatchKnownKeys)
	if v, ok := m["enabled"]; ok {
		var enabled bool
		issues = append(issues, checkType("dispatch.enabled", "boolean", v, &enabled)...)
	}
	if v, ok := m["serverUrl"]; ok {
		serverURL, urlIssues := parseURL("dispatch.serverUrl", v)
		issues = append(issues, urlIssues...)
		out.ServerURL = serverURL
	}
	if v, ok := m["token"]; ok {
		var token string
		issues = append(issues, checkType("dispatch.token", "string", v, &token)...)
	}
	return out, issues
}

func parseEnvoy(raw json.RawMessage) (EnvoySection, []string) {
	m, issues := section("envoy", raw)
	if issues != nil {
		return EnvoySection{}, issues
	}
	var out EnvoySection
	issues = unknownKeys("envoy", m, envoyKnownKeys)
	if v, ok := m["enabled"]; ok {
		var enabled bool
		issues = append(issues, checkType("envoy.enabled", "boolean", v, &enabled)...)
	}
	if v, ok := m["url"]; ok {
		_, urlIssues := parseURL("envoy.url", v)
		issues = append(issues, urlIssues...)
	}
	if v, ok := m["natsUrls"]; ok {
		var natsURLs []string
		issues = append(issues, checkType("envoy.natsUrls", "string array", v, &natsURLs)...)
		for i, natsURL := range natsURLs {
			natsURLs[i] = strings.TrimSpace(natsURL)
			if natsURLs[i] == "" {
				issues = append(issues, fmt.Sprintf("envoy.natsUrls.%d: must not be empty", i))
			}
		}
		out.NatsURLs = natsURLs
	}
	for _, key := range []string{"token", "natsSeed"} {
		if v, ok := m[key]; ok {
			var s string
			issues = append(issues, checkType("envoy."+key, "string", v, &s)...)
		}
	}
	return out, issues
}

// mergeConfig overlays a checkout's file on the user file: its dispatch.serverUrl, when it names
// one. A checkout's file holds no envoy section (parseAndValidate), so the user file's stands.
func mergeConfig(base, override *EnvoyConfig) *EnvoyConfig {
	out := *base
	if override.Dispatch != nil && override.Dispatch.ServerURL != "" {
		out.Dispatch = &DispatchConfig{ServerURL: override.Dispatch.ServerURL}
	}
	return &out
}
