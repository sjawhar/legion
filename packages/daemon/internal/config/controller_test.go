package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// controllerFile writes a controller.yaml holding body into a directory of the test's own and
// returns its path.
func controllerFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the controller file: %v", err)
	}
	return path
}

// requiredControllerKeys are the five keys `legion controller start` cannot run without.
const requiredControllerKeys = `project: demo
daemon_url: http://daemon.test:13370
operator_token_file: ./operator-token
envoy_url: http://envoy.test:9020
nats_urls: [nats://a:4222]
`

// Every path key resolves against the file's own directory, `~` included (no tilde expansion, the
// daemon loader's rule); the Dispatch URL is a base URL; nats_urls is a set; the daemon URL loses
// its trailing slash.
func TestLoadControllerReadsEveryKey(t *testing.T) {
	path := controllerFile(t, `project: demo
daemon_url: http://daemon.test:13370/
operator_token_file: operator-token
envoy_url: http://envoy.test:9020
envoy_token_file: ./envoy-token
nats_urls: [nats://a:4222, nats://b:4222, nats://a:4222]
nats_nkey_seed_file: secrets/legion-pane.nk
dispatch_url: https://d.test/
dispatch_token_file: ~/dispatch-token
instructions: ./instructions.md
omp_invocation: mise x github:sjawhar/oh-my-pi@1 -- omp
omp_launch_prefix: [secrets, ANTHROPIC_API_KEY, --]
state_dir: ./state
`)
	dir := filepath.Dir(path)

	got, err := LoadController(path, "")
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}
	want := ControllerConfig{
		Project:           "demo",
		DaemonURL:         "http://daemon.test:13370",
		OperatorTokenFile: filepath.Join(dir, "operator-token"),
		EnvoyURL:          "http://envoy.test:9020",
		EnvoyTokenFile:    filepath.Join(dir, "envoy-token"),
		NatsURLs:          []string{"nats://a:4222", "nats://b:4222"},
		NatsNkeySeedFile:  filepath.Join(dir, "secrets", "legion-pane.nk"),
		DispatchURL:       "https://d.test",
		DispatchTokenFile: filepath.Join(dir, "~", "dispatch-token"),
		InstructionsPath:  filepath.Join(dir, "instructions.md"),
		OmpInvocation:     "mise x github:sjawhar/oh-my-pi@1 -- omp",
		OmpLaunchPrefix:   []string{"secrets", "ANTHROPIC_API_KEY", "--"},
		StateDir:          filepath.Join(dir, "state"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadController =\n%+v\nwant\n%+v", got, want)
	}
}

// `--daemon-url` wins over the file's daemon_url, validated and trimmed the same way. It keeps a
// base path because the controller appends its routes there, but refuses a query or fragment.
func TestLoadControllerTakesTheDaemonURLOverride(t *testing.T) {
	path := controllerFile(t, requiredControllerKeys)
	got, err := LoadController(path, "http://127.0.0.1:13370/tenant//")
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}
	if got.DaemonURL != "http://127.0.0.1:13370/tenant" {
		t.Fatalf("DaemonURL = %q, want the override with its base path and no trailing slashes", got.DaemonURL)
	}
	for _, tc := range []struct {
		override, want string
	}{
		{"not a url", "--daemon-url must be a valid URL"},
		{"http://127.0.0.1:13370?token=token", "--daemon-url must not include a query string or fragment"},
		{"http://127.0.0.1:13370#fragment", "--daemon-url must not include a query string or fragment"},
	} {
		if _, err := LoadController(path, tc.override); err == nil || err.Error() != tc.want {
			t.Errorf("override %q error = %v, want %q", tc.override, err, tc.want)
		}
	}
}

// The value an operator copies from the cluster's legion.yaml is sanitized exactly as the daemon
// sanitizes its own project, so both name the same controller token.
func TestLoadControllerSanitizesTheProjectAsTheDaemonDoes(t *testing.T) {
	path := controllerFile(t, strings.Replace(requiredControllerKeys, "project: demo", "project: sjawhar/Legion", 1))
	got, err := LoadController(path, "")
	if err != nil {
		t.Fatalf("LoadController: %v", err)
	}
	if got.Project != "sjawharlegion" {
		t.Fatalf("Project = %q, want sjawharlegion", got.Project)
	}
}

func TestLoadControllerRefuses(t *testing.T) {
	without := func(key string) string {
		var kept []string
		for _, line := range strings.Split(requiredControllerKeys, "\n") {
			if !strings.HasPrefix(line, key+":") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"a file that is not a mapping", "- a\n- b\n", "controller.yaml must be a mapping"},
		{"an unknown key", requiredControllerKeys + "runtime: kubernetes\n",
			`unknown key "runtime" in the controller configuration; legion controller start reads only project, daemon_url, operator_token_file, envoy_url, envoy_token_file, nats_urls, nats_nkey_seed_file, dispatch_url, dispatch_token_file, instructions, omp_invocation, omp_launch_prefix, state_dir — see deploy/kubernetes/daemon/controller.yaml.example`},
		{"no project", without("project"), "project is required in the controller configuration"},
		{"no daemon_url", without("daemon_url"), "daemon_url is required in the controller configuration"},
		{"no operator_token_file", without("operator_token_file"), "operator_token_file is required in the controller configuration"},
		{"no envoy_url", without("envoy_url"), "envoy_url is required in the controller configuration"},
		{"no nats_urls", without("nats_urls"), "nats_urls is required in the controller configuration"},
		{"empty nats_urls", without("nats_urls") + "nats_urls: []\n", "nats_urls is required in the controller configuration"},
		{"a blank project", strings.Replace(requiredControllerKeys, "project: demo", `project: " "`, 1), "project must not be empty"},
		{"a project with no alphanumeric character", strings.Replace(requiredControllerKeys, "project: demo", `project: "/-_/"`, 1),
			`project "/-_/" must include at least one alphanumeric character`},
		{"a broken daemon_url", strings.Replace(requiredControllerKeys, "http://daemon.test:13370", "daemon", 1), "daemon_url must be a valid URL"},
		{"a broken envoy_url", strings.Replace(requiredControllerKeys, "http://envoy.test:9020", "envoy", 1), "envoy_url must be a valid URL"},
		{"dispatch_url alone", requiredControllerKeys + "dispatch_url: https://d.test\n", "dispatch_token_file is required when dispatch_url is configured"},
		{"a daemon_url with a query string", strings.Replace(requiredControllerKeys, "http://daemon.test:13370", "http://daemon.test:13370?token=token", 1),
			"daemon_url must not include a query string or fragment"},
		{"a daemon_url with a fragment", strings.Replace(requiredControllerKeys, "http://daemon.test:13370", "http://daemon.test:13370#fragment", 1),
			"daemon_url must not include a query string or fragment"},
		{"a broken nats_urls entry", without("nats_urls") + "nats_urls: [nats]\n", `nats_urls entry "nats" must be a valid URL`},
		{"an envoy_url with a query string", strings.Replace(requiredControllerKeys, "http://envoy.test:9020", "https://envoy.test?access_token=token", 1),
			"envoy_url must not include a query string or fragment; use URL userinfo or a secret for credentials"},
		{"an envoy_url with a fragment", strings.Replace(requiredControllerKeys, "http://envoy.test:9020", "https://envoy.test#fragment", 1),
			"envoy_url must not include a query string or fragment; use URL userinfo or a secret for credentials"},
		{"a nats_urls entry with a query string", strings.Replace(requiredControllerKeys, "nats_urls: [nats://a:4222]", `nats_urls: ["nats://a:4222?token=token"]`, 1),
			`nats_urls entry "nats://a:4222?token=token" must not include a query string or fragment; use URL userinfo or a secret for credentials`},
		{"a nats_urls entry with a fragment", strings.Replace(requiredControllerKeys, "nats_urls: [nats://a:4222]", `nats_urls: ["nats://a:4222#fragment"]`, 1),
			`nats_urls entry "nats://a:4222#fragment" must not include a query string or fragment; use URL userinfo or a secret for credentials`},
		{"dispatch_token_file alone", requiredControllerKeys + "dispatch_token_file: ./t\n", "dispatch_token_file is set but dispatch_url is not"},
		{"the Dispatch MCP endpoint", requiredControllerKeys + "dispatch_url: https://d.test/mcp/\ndispatch_token_file: ./t\n",
			"dispatch_url must be the dispatch service base URL, not the /mcp endpoint"},
		{"a blank omp_invocation", requiredControllerKeys + "omp_invocation: \"\"\n", "omp_invocation must not be empty"},
		{"an omp_launch_prefix that is not argv", requiredControllerKeys + "omp_launch_prefix: secrets\n", "omp_launch_prefix must be an array of non-empty strings"},
		{"a blank path key", requiredControllerKeys + "envoy_token_file: \"\"\n", "envoy_token_file must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadController(controllerFile(t, tc.body), "")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v\nwant %s", err, tc.want)
			}
		})
	}
}

// Two unknown keys are both named, in file order.
func TestLoadControllerNamesEveryUnknownKey(t *testing.T) {
	_, err := LoadController(controllerFile(t, requiredControllerKeys+"runtime: tmux\nport: 1\n"), "")
	if err == nil || !strings.Contains(err.Error(), `unknown key "runtime"`) || !strings.Contains(err.Error(), `; unknown key "port"`) {
		t.Fatalf("err = %v, want both unknown keys named", err)
	}
}

// The example every refusal points at loads, so an operator who copies it starts from a file the
// command accepts.
func TestLoadControllerReadsTheShippedExample(t *testing.T) {
	example := filepath.Join("..", "..", "..", "..", ControllerConfigExample)
	got, err := LoadController(example, "")
	if err != nil {
		t.Fatalf("LoadController(%s): %v", example, err)
	}
	if got.Project != "demo" || got.DaemonURL != "http://127.0.0.1:13370" || len(got.NatsURLs) != 1 {
		t.Fatalf("LoadController(%s) = %+v", example, got)
	}
}
