// Package modelroute is how Oh My Pi in a Legion pod reaches its models: through the model gateway
// alone (LEGION-208 Stage 4b plan, C6; LEGION-199 design A′). The image's `legion` profile cannot
// carry the route, because Oh My Pi reads a models.yml baseUrl literally and the gateway's URL is
// the daemon's configuration, handed to each pod as LEGION_MODEL_GATEWAY_URL. So the Go `legion`
// writes the profile's model configuration when Oh My Pi starts in a pod — the worker shim before
// it spawns the agent, `legion probe-image` before its model round trip — from two embedded files:
// config.yml, which pins every role, subagent and retry to the gateway's aliases, and
// models.yml.tmpl, which routes the anthropic provider to the gateway and keys it with the pod's
// projected token. The pins also reach Oh My Pi as a settings overlay (Installed.Environ), which
// outranks the settings of the repository the agent works in.
package modelroute

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

const (
	// EnvURL is the variable a pod's main container carries the gateway's base URL in
	// (sandbox.mainEnvironment); its anthropic route is <URL>/anthropic.
	EnvURL = "LEGION_MODEL_GATEWAY_URL"
	// TokenFile is the pod's projected gateway token (sandbox.GatewayDir), which the profile's key
	// command reads.
	TokenFile = "/var/run/legion/gateway/token"
	// DefaultModel is the model config.yml's default role runs: every phase worker's own session,
	// and the one the image probe's round trip must be answered by.
	DefaultModel = "anthropic/claude-fable-5-1-legion"
)

var (
	//go:embed config.yml
	config []byte
	//go:embed models.yml.tmpl
	modelsSource string
	models       = template.Must(template.New("models.yml").Funcs(template.FuncMap{"yaml": yamlScalar}).Parse(modelsSource))
	// profileName is a profile name Oh My Pi accepts (@oh-my-pi/pi-utils src/dirs.ts:38).
	profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Installed is what Install wrote: the anthropic route, <gateway>/anthropic, and the profile's
// config.yml, which holds the pins. The zero value is no route installed.
type Installed struct {
	Route, Pins string
}

// pinsVariable is Oh My Pi's settings-overlay path list. Oh My Pi resolves settings as defaults,
// then the profile's config.yml, then the working directory's project settings (.omp/config.yml
// and the other providers' settings files), then these overlays, then `--config` overlays, then
// runtime overrides; a list setting at a higher layer replaces the one below
// (docs/config-usage.md, "Settings resolution model", and Settings in
// packages/coding-agent/src/config/settings.ts, at the pinned fork tag).
const pinsVariable = "PI_CONFIG_FILES"

// foundryVariable turns Anthropic Foundry on, which puts FOUNDRY_BASE_URL ahead of a model's own
// baseUrl for every anthropic request (resolveDirectAnthropicBaseUrl in
// packages/ai/src/providers/anthropic-state.ts, at the pinned fork tag). Oh My Pi fills a variable
// from the working directory's .env only when it is unset or empty, so a pod holding it off keeps a
// repository's .env from moving the route.
const foundryVariable = "CLAUDE_CODE_USE_FOUNDRY"

// Environ is environ as the Oh My Pi it starts gets it: with the pins last among the settings
// overlays (PI_CONFIG_FILES; later files win), so for every single-value or list setting the pins
// hold — disabledProviders, enabledModels, retry.modelFallback, the endpoints Oh My Pi posts to on
// its own, and each role they name — a repository's own settings cannot override them, and with
// Anthropic Foundry held off (foundryVariable), so a repository's .env cannot move the route. A
// record merges key by key, so a repository can add keys the pins do not set (another fallback
// chain, another role); fallback is off in the pins for that reason. It is environ unchanged when
// nothing was installed (a tmux pane, the image build).
func (i Installed) Environ(environ []string) []string {
	if i.Pins == "" {
		return environ
	}
	out := make([]string, 0, len(environ)+2)
	overlays := i.Pins
	for _, pair := range environ {
		if value, ok := strings.CutPrefix(pair, pinsVariable+"="); ok {
			if value != "" {
				overlays = value + string(os.PathListSeparator) + i.Pins
			}
			continue
		}
		if strings.HasPrefix(pair, foundryVariable+"=") {
			continue
		}
		out = append(out, pair)
	}
	return append(out, foundryVariable+"=0", pinsVariable+"="+overlays)
}

// Install writes the model route into the Oh My Pi profile the environment names
// (<HOME>/.omp/profiles/<OMP_PROFILE>/agent: config.yml and models.yml, replacing either) when the
// environment names a gateway, and answers what it wrote. Without LEGION_MODEL_GATEWAY_URL — a
// tmux pane, the image build — it leaves the profile alone and answers the zero Installed. A
// gateway that is not an http(s) base URL, or a profile it cannot locate, is refused naming the
// variable, with nothing written.
func Install(lookup func(string) (string, bool)) (Installed, error) {
	return InstallKeyedBy(lookup, TokenFile)
}

// InstallKeyedBy is Install keyed by the token at tokenFile rather than the pod's: the real-binary
// tests, here and in internal/daemon, run Oh My Pi on the route with a token file they can write.
func InstallKeyedBy(lookup func(string) (string, bool), tokenFile string) (Installed, error) {
	raw, set := lookup(EnvURL)
	if !set {
		return Installed{}, nil
	}
	route, err := anthropicRoute(raw)
	if err != nil {
		return Installed{}, err
	}
	agent, err := agentDir(lookup)
	if err != nil {
		return Installed{}, fmt.Errorf("%s is set, and the Oh My Pi profile to route cannot be found: %w", EnvURL, err)
	}
	var rendered bytes.Buffer
	if err := models.Execute(&rendered, struct{ BaseURL, KeyCommand string }{route, "!cat " + tokenFile}); err != nil {
		return Installed{}, fmt.Errorf("render the profile's models.yml: %w", err)
	}
	if err := os.MkdirAll(agent, 0o700); err != nil {
		return Installed{}, fmt.Errorf("create the Oh My Pi profile directory: %w", err)
	}
	for name, content := range map[string][]byte{"config.yml": config, "models.yml": rendered.Bytes()} {
		if err := os.WriteFile(filepath.Join(agent, name), content, 0o600); err != nil {
			return Installed{}, fmt.Errorf("write the profile's model route: %w", err)
		}
	}
	return Installed{Route: route, Pins: filepath.Join(agent, "config.yml")}, nil
}

// anthropicRoute is the gateway's anthropic route, <gateway>/anthropic, for a gateway base URL that
// is http(s) with a host and carries no credentials, query or fragment. A refusal never quotes a
// URL's password.
func anthropicRoute(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is set but empty: it must name the model gateway's base URL", EnvURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		var parseErr *url.Error
		if errors.As(err, &parseErr) {
			err = parseErr.Err
		}
		return "", fmt.Errorf("%s is not an http(s) URL with a host: %w", EnvURL, err)
	}
	switch {
	case u.User != nil:
		return "", fmt.Errorf("%s carries credentials; the gateway's key is the pod's projected token, never a URL's", EnvURL)
	case (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return "", fmt.Errorf("%s %q is not an http(s) URL with a host", EnvURL, raw)
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "?#"):
		return "", fmt.Errorf("%s %q carries a query or fragment: it must be the gateway's base URL", EnvURL, raw)
	}
	return u.JoinPath("anthropic").String(), nil
}

// agentDir is Oh My Pi's agent directory for the named profile under HOME
// (@oh-my-pi/pi-utils src/dirs.ts: `.omp/profiles/<name>` under the home directory, then `agent`).
// The route belongs to a named profile: the image's is `legion`.
func agentDir(lookup func(string) (string, bool)) (string, error) {
	home, _ := lookup("HOME")
	if home == "" {
		return "", errors.New("HOME is not set")
	}
	profile, _ := lookup("OMP_PROFILE")
	profile = strings.TrimSpace(profile)
	switch {
	case profile == "" || profile == "default":
		return "", errors.New("OMP_PROFILE names no profile; the route is written to a named profile (the image's is legion)")
	case !profileName.MatchString(profile) || strings.HasSuffix(profile, "."):
		return "", fmt.Errorf("OMP_PROFILE %q is not a profile name", profile)
	}
	return filepath.Join(home, ".omp", "profiles", profile, "agent"), nil
}

// yamlScalar is s as a YAML scalar, quoted as YAML requires.
func yamlScalar(s string) (string, error) {
	encoded, err := yaml.Marshal(s)
	return strings.TrimSuffix(string(encoded), "\n"), err
}
