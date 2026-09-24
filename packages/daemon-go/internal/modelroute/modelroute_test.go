package modelroute

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// profile is what the tests read back from the two files Install writes.
type profile struct {
	Models struct {
		Providers map[string]struct {
			BaseURL string            `yaml:"baseUrl"`
			Auth    string            `yaml:"auth"`
			APIKey  string            `yaml:"apiKey"`
			Headers map[string]string `yaml:"headers"`
			Models  []struct {
				ID string `yaml:"id"`
			} `yaml:"models"`
		} `yaml:"providers"`
	}
	Config struct {
		EnabledModels     []string          `yaml:"enabledModels"`
		DisabledProviders []string          `yaml:"disabledProviders"`
		ModelRoles        map[string]string `yaml:"modelRoles"`
		Retry             struct {
			FallbackChains map[string][]string `yaml:"fallbackChains"`
			ModelFallback  *bool               `yaml:"modelFallback"`
		} `yaml:"retry"`
	}
}

// environment is a pod's: a HOME, the image's OMP_PROFILE, and the gateway the daemon names.
func environment(t *testing.T, gateway string) map[string]string {
	t.Helper()
	return map[string]string{"HOME": t.TempDir(), "OMP_PROFILE": "legion", EnvURL: gateway}
}

func lookup(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
}

func readProfile(t *testing.T, home string) profile {
	t.Helper()
	agent := filepath.Join(home, ".omp", "profiles", "legion", "agent")
	var p profile
	for file, into := range map[string]any{"models.yml": &p.Models, "config.yml": &p.Config} {
		raw, err := os.ReadFile(filepath.Join(agent, file))
		if err != nil {
			t.Fatalf("read the profile's %s: %v", file, err)
		}
		if err := yaml.Unmarshal(raw, into); err != nil {
			t.Fatalf("%s is not YAML: %v\n%s", file, err, raw)
		}
	}
	return p
}

// A pod's Oh My Pi reaches its models through the gateway the daemon names and nowhere else: the
// anthropic provider is routed to `<gateway>/anthropic`, keyed by the projected token through a key
// command Oh My Pi re-runs on a 401, and shaped as an API-key caller; every model a session, a
// role, a subagent or a retry can pick is a `-legion` alias the gateway serves.
func TestInstallRoutesTheProfileThroughTheGateway(t *testing.T) {
	for gateway, base := range map[string]string{
		"https://middleman.hawk.internal.trajectorylabs.com":  "https://middleman.hawk.internal.trajectorylabs.com/anthropic",
		"https://middleman.hawk.internal.trajectorylabs.com/": "https://middleman.hawk.internal.trajectorylabs.com/anthropic",
		"http://10.1.20.250:8080/gateway":                     "http://10.1.20.250:8080/gateway/anthropic",
	} {
		t.Run(gateway, func(t *testing.T) {
			env := environment(t, gateway)

			installed, err := Install(lookup(env))

			pins := filepath.Join(env["HOME"], ".omp", "profiles", "legion", "agent", "config.yml")
			if err != nil || installed != (Installed{Route: base, Pins: pins}) {
				t.Fatalf("Install = %+v, %v; want the route %s and the pins %s", installed, err, base, pins)
			}
			p := readProfile(t, env["HOME"])
			anthropic, ok := p.Models.Providers["anthropic"]
			if !ok || len(p.Models.Providers) != 1 {
				t.Fatalf("models.yml configures providers %v, want anthropic alone", slices.Collect(maps.Keys(p.Models.Providers)))
			}
			key := "!cat " + TokenFile
			if anthropic.BaseURL != base || anthropic.APIKey != key || anthropic.Headers["X-Api-Key"] != key || anthropic.Auth != "apiKey" {
				t.Errorf("anthropic = baseUrl %q, apiKey %q, X-Api-Key %q, auth %q; want %q, %q twice, apiKey",
					anthropic.BaseURL, anthropic.APIKey, anthropic.Headers["X-Api-Key"], anthropic.Auth, base, key)
			}
			declared := map[string]bool{}
			for _, model := range anthropic.Models {
				if !strings.HasSuffix(model.ID, "-legion") {
					t.Errorf("models.yml declares %s, which the gateway does not serve Legion's pods", model.ID)
				}
				declared["anthropic/"+model.ID] = true
			}
			if !slices.Equal(p.Config.EnabledModels, []string{"anthropic/*-legion"}) {
				t.Errorf("enabledModels = %v, want the gateway's aliases alone", p.Config.EnabledModels)
			}
			// Every role, and every model a retry falls back to, is a declared alias: a selector
			// naming anything else is a model no pod can reach.
			names := func(selector string) string { name, _, _ := strings.Cut(selector, ":"); return name }
			for role, selector := range p.Config.ModelRoles {
				if !declared[names(selector)] {
					t.Errorf("role %s runs %s, which models.yml does not declare", role, selector)
				}
			}
			// No failed turn falls back to another model: a repository can add fallback chains (a
			// record merges key by key), and Oh My Pi resolves a candidate past disabledProviders.
			if p.Config.Retry.ModelFallback == nil || *p.Config.Retry.ModelFallback {
				t.Error("retry.modelFallback is not pinned false")
			}
			if names(p.Config.ModelRoles["default"]) != DefaultModel {
				t.Errorf("the default role runs %s, want DefaultModel %s", p.Config.ModelRoles["default"], DefaultModel)
			}
			for from, chain := range p.Config.Retry.FallbackChains {
				for _, to := range append([]string{from}, chain...) {
					if !declared[names(to)] {
						t.Errorf("the fallback chain %s names %s, which models.yml does not declare", from, to)
					}
				}
			}
			// The providers that answer with no key of the gateway's: Amazon Bedrock and Vertex from
			// ambient cloud credentials, the local servers from nothing at all.
			for _, provider := range []string{"amazon-bedrock", "bedrock-mantle", "google-vertex", "ollama", "llama.cpp", "lm-studio"} {
				if !slices.Contains(p.Config.DisabledProviders, provider) {
					t.Errorf("disabledProviders %v leaves %s enabled", p.Config.DisabledProviders, provider)
				}
			}
		})
	}
}

// Without a gateway (a tmux pane, or a build) the profile is left as it is.
func TestInstallLeavesTheProfileAloneWithoutAGateway(t *testing.T) {
	env := environment(t, "")
	delete(env, EnvURL)

	installed, err := Install(lookup(env))

	if err != nil || installed != (Installed{}) {
		t.Fatalf("Install = %+v, %v; want nothing installed", installed, err)
	}
	if _, err := os.Stat(filepath.Join(env["HOME"], ".omp")); !os.IsNotExist(err) {
		t.Errorf("Install wrote under HOME without a gateway: %v", err)
	}
}

// A gateway that is no http(s) base URL, or a profile Install cannot locate, is refused naming the
// variable, and nothing is written: Oh My Pi would otherwise start on whatever the profile held.
func TestInstallRefusesWhatItCannotRoute(t *testing.T) {
	for name, testCase := range map[string]struct {
		edit func(map[string]string)
		want string
	}{
		"an empty gateway":     {func(env map[string]string) { env[EnvURL] = "" }, EnvURL + " is set but empty"},
		"a relative gateway":   {func(env map[string]string) { env[EnvURL] = "middleman.internal" }, EnvURL + ` "middleman.internal" is not an http(s) URL with a host`},
		"another scheme":       {func(env map[string]string) { env[EnvURL] = "ftp://middleman.internal" }, "not an http(s) URL"},
		"a query":              {func(env map[string]string) { env[EnvURL] = "https://middleman.internal/?x=1" }, "carries a query or fragment"},
		"a fragment":           {func(env map[string]string) { env[EnvURL] = "https://middleman.internal/#x" }, "carries a query or fragment"},
		"credentials":          {func(env map[string]string) { env[EnvURL] = "https://user:secret@middleman.internal" }, "carries credentials"},
		"no HOME":              {func(env map[string]string) { delete(env, "HOME") }, "HOME is not set"},
		"no profile":           {func(env map[string]string) { delete(env, "OMP_PROFILE") }, "OMP_PROFILE names no profile"},
		"the default profile":  {func(env map[string]string) { env["OMP_PROFILE"] = "default" }, "OMP_PROFILE names no profile"},
		"a path as a profile":  {func(env map[string]string) { env["OMP_PROFILE"] = "../legion" }, `OMP_PROFILE "../legion" is not a profile name`},
		"a gateway on a space": {func(env map[string]string) { env[EnvURL] = "https://middle man.internal" }, "is not an http(s) URL with a host"},
	} {
		t.Run(name, func(t *testing.T) {
			env := environment(t, "https://middleman.internal")
			home := env["HOME"]
			testCase.edit(env)

			installed, err := Install(lookup(env))

			if err == nil || installed != (Installed{}) || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Install = %+v, %v; want a refusal saying %q", installed, err, testCase.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the refusal quotes the URL's password: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, ".omp")); !os.IsNotExist(err) {
				t.Errorf("a refused Install wrote under HOME: %v", err)
			}
		})
	}
}

// The Oh My Pi a pod starts gets the pins as its last settings overlay, so they outrank the
// repository's settings and any overlay the pod already names; with nothing installed (a tmux
// pane) its environment is unchanged.
func TestEnvironPutsThePinsLastAmongTheOverlays(t *testing.T) {
	installed := Installed{Route: "https://gw/anthropic", Pins: "/home/legion/.omp/profiles/legion/agent/config.yml"}
	for name, testCase := range map[string]struct {
		environ []string
		want    []string
	}{
		"no overlay yet":   {[]string{"HOME=/home/legion"}, []string{"HOME=/home/legion", "PI_CONFIG_FILES=" + installed.Pins}},
		"an overlay set":   {[]string{"PI_CONFIG_FILES=/etc/omp.yml", "HOME=/home/legion"}, []string{"HOME=/home/legion", "PI_CONFIG_FILES=/etc/omp.yml:" + installed.Pins}},
		"an empty one set": {[]string{"PI_CONFIG_FILES=", "HOME=/home/legion"}, []string{"HOME=/home/legion", "PI_CONFIG_FILES=" + installed.Pins}},
	} {
		if got := installed.Environ(testCase.environ); !slices.Equal(got, testCase.want) {
			t.Errorf("%s: Environ = %q, want %q", name, got, testCase.want)
		}
	}
	unchanged := []string{"HOME=/home/ubuntu", "PI_CONFIG_FILES=/etc/omp.yml"}
	if got := (Installed{}).Environ(unchanged); !slices.Equal(got, unchanged) {
		t.Errorf("Environ with nothing installed = %q, want %q", got, unchanged)
	}
}

// config.yml's disabledProviders is derived from the Oh My Pi fork release it names; a pin bump
// can add providers, so the list is re-derived whenever omp-pin.ts moves.
func TestTheClosedProviderSetMatchesThePinnedOhMyPi(t *testing.T) {
	pinFile, err := os.ReadFile(filepath.Join("..", "..", "..", "daemon", "src", "daemon", "omp-pin.ts"))
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`OMP_FORK_PIN = "([^"]+)"`).FindSubmatch(pinFile)
	derived := regexp.MustCompile(`(?m)^# derived at: (\S+)$`).FindSubmatch(config)
	if pin == nil || derived == nil || string(pin[1]) != string(derived[1]) {
		t.Fatalf("config.yml's disabledProviders was derived at %q, but omp-pin.ts pins %q: re-derive it (config.yml says how)", derived, pin)
	}
}
