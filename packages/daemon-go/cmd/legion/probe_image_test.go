package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// imageOmp is an `omp` that passes the image's three probes as a working image's Oh My Pi does:
// pi.agents is there, the plugin linked into the profile loads from its own package and finds every
// task agent and skill Legion's prompts name, and the session-storage setting refuses a value it
// does not know, naming the variable. Asked for task agents, it resolves their models too, unless
// told to skip them. With $LEGION_TEST_SEEN set, each run appends the environment it saw there:
// PI_CONFIG_FILES, whether the first overlay it names exists, and OTEL_SDK_DISABLED; with
// $LEGION_TEST_KEY_SEEN, the provider key TEST_PROVIDER_KEY.
func imageOmp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp")
	script := `#!/bin/sh
if [ -n "${LEGION_TEST_SEEN:-}" ]; then
  first=${PI_CONFIG_FILES%%:*}
  if [ -n "$first" ] && [ -f "$first" ]; then written=written; else written=absent; fi
  printf '%s %s %s\n' "${PI_CONFIG_FILES:-none}" "$written" "${OTEL_SDK_DISABLED:-unset}" >>"$LEGION_TEST_SEEN"
fi
[ -z "${LEGION_TEST_KEY_SEEN:-}" ] || printf '%s\n' "${TEST_PROVIDER_KEY:-unset}" >>"$LEGION_TEST_KEY_SEEN"
installed=$(cd "$HOME/.omp/profiles/$OMP_PROFILE/plugins/node_modules/@sjawhar/pi-legion-envoy" && pwd -P)
case "$*" in
"models --no-extensions --extension "*" --json") echo LEGION_OMP_AGENTS=available >&2 ;;
"models --extension "*" --json")
  printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/dist/legion.js\n' "$installed" >&2
  if [ -n "${LEGION_PROMPT_AGENTS:-}" ]; then echo LEGION_PROMPT_AGENTS=resolved >&2; fi
  if [ -n "${LEGION_PROMPT_AGENTS:-}" ] && [ -z "${LEGION_SKIP_AGENT_MODELS:-}" ]; then echo LEGION_AGENT_MODELS=resolved >&2; fi
  if [ -n "${LEGION_PROMPT_SKILLS:-}" ]; then echo LEGION_PROMPT_SKILLS=resolved >&2; fi ;;
*) echo "Invalid $OMP_SESSION_STORAGE for OMP_SESSION_STORAGE" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	return path
}

// thisBinarysContract is the Go daemon API contract this binary speaks, as a manifest writes it.
var thisBinarysContract = strconv.Itoa(api.GoDaemonAPIVersion)

// inImage sets this process's environment to the worker image's: a HOME whose `legion` profile
// links the plugin, its manifest declaring Go contract goContract, and LEGION_OMP_PATH set to omp.
func inImage(t *testing.T, goContract, omp string) {
	t.Helper()
	home := t.TempDir()
	unpacked := filepath.Join(home, "pi-legion-envoy")
	if err := os.MkdirAll(unpacked, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@sjawhar/pi-legion-envoy","version":"1.57.0","legion":{"daemonApiVersion":8,"goDaemonApiVersion":` + goContract + `}}`
	if err := os.WriteFile(filepath.Join(unpacked, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(home, ".omp", "profiles", "legion", "plugins", "node_modules", "@sjawhar", "pi-legion-envoy")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unpacked, installed); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OMP_PROFILE", "legion")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LEGION_OMP_PATH", omp)
	t.Chdir(t.TempDir())
}

func probeImage(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "probe-image"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Bare, as the worker image's build runs it, the command holds the image's plugin to the contract
// this binary speaks and prints the OK line the daemon's probe Sandbox reads.
func TestProbeImagePrintsTheOKLineWithThisBinarysContract(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, thisBinarysContract, omp)

	code, stdout, stderr := probeImage()

	if code != 0 {
		t.Fatalf("probe-image exited %d: %s", code, stderr)
	}
	if want := "probe-image: OK (" + omp + ") session-storage=probed agent-models=resolved go-daemon-api-version=" + thisBinarysContract + "\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

// The daemon's probe Sandbox passes its own contract, which the image's plugin must declare.
func TestProbeImageHoldsThePluginToTheContractItIsAskedFor(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, "2", omp)

	code, stdout, stderr := probeImage("--go-daemon-api-version", "2")
	if code != 0 || !strings.HasSuffix(stdout, " go-daemon-api-version=2\n") {
		t.Fatalf("probe-image --go-daemon-api-version 2 over a plugin declaring 2 = %d %q %q, want the OK line confirming 2", code, stdout, stderr)
	}

	code, stdout, stderr = probeImage("--go-daemon-api-version", "4")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "speaks Go daemon API contract 2; this daemon requires 4") {
		t.Fatalf("probe-image --go-daemon-api-version 4 over a plugin declaring 2 = %d %q %q, want exit 1 naming both contracts and no OK line", code, stdout, stderr)
	}
}

func TestProbeImageProbesTheOmpItIsGiven(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, thisBinarysContract, "/nonexistent/omp")

	code, stdout, stderr := probeImage("--omp", omp)

	if code != 0 || stdout != "probe-image: OK ("+omp+") session-storage=probed agent-models=resolved go-daemon-api-version="+thisBinarysContract+"\n" {
		t.Fatalf("probe-image --omp = %d %q %q, want the OK line naming %s", code, stdout, stderr, omp)
	}
}

// The image build's probe, which has none of the operator's model configuration, skips the agents'
// models, and its OK line says so, which the daemon's probe Sandbox refuses at boot.
func TestProbeImageWithSkipAgentModelsSaysSoOnTheOKLine(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, thisBinarysContract, omp)

	code, stdout, stderr := probeImage("--skip-agent-models")

	if want := "probe-image: OK (" + omp + ") session-storage=probed agent-models=skipped go-daemon-api-version=" + thisBinarysContract + "\n"; code != 0 || stdout != want {
		t.Fatalf("probe-image --skip-agent-models = %d %q %q, want %q", code, stdout, stderr, want)
	}
}

// With --provider-env-dir, as the probe Sandbox runs it when provider keys are configured, every
// probe's Oh My Pi gets each key as a worker's shim exports it, so an agent keyed only through the
// providers Secret resolves as it would in a worker; a key the environment already names is refused
// as the shim refuses it, before any probe runs.
func TestProbeImageWithAProviderEnvDirExportsTheKeysAsTheShimDoes(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, thisBinarysContract, omp)
	providers := t.TempDir()
	if err := os.WriteFile(filepath.Join(providers, "TEST_PROVIDER_KEY"), []byte("from-the-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(t.TempDir(), "seen")
	t.Setenv("LEGION_TEST_KEY_SEEN", seen)

	code, stdout, stderr := probeImage("--provider-env-dir", providers)

	if code != 0 || !strings.HasPrefix(stdout, "probe-image: OK (") {
		t.Fatalf("probe-image --provider-env-dir = %d %q %q, want the OK line", code, stdout, stderr)
	}
	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range strings.Fields(string(raw)) {
		if key != "from-the-secret" {
			t.Errorf("a probe's Oh My Pi had TEST_PROVIDER_KEY=%q, want the Secret's", key)
		}
	}
	if len(strings.Fields(string(raw))) < 3 {
		t.Fatalf("Oh My Pi ran %d times, want the three probes: %q", len(strings.Fields(string(raw))), raw)
	}

	t.Setenv("TEST_PROVIDER_KEY", "already-set")
	if err := os.Remove(seen); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = probeImage("--provider-env-dir", providers)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "TEST_PROVIDER_KEY") {
		t.Fatalf("probe-image --provider-env-dir over a set TEST_PROVIDER_KEY = %d %q %q, want exit 1 naming the key", code, stdout, stderr)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("Oh My Pi ran before the refusal (%v)", err)
	}
}

// With --pod-safety, as the daemon's probe Sandbox runs it, every probe runs Oh My Pi on the pod's
// baseline, as a pod's shim starts it: the overlay written and named first in PI_CONFIG_FILES,
// ahead of the pod's own, and OpenTelemetry held off. Bare, as the image's build runs it, the
// probes run on the environment as it is.
func TestProbeImageWithPodSafetyProbesOnThePodsBaseline(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, thisBinarysContract, omp)
	t.Setenv("PI_CONFIG_FILES", "/etc/legion-operator/overlay.yml")
	t.Setenv("OTEL_SDK_DISABLED", "")
	for name, tc := range map[string]struct {
		args []string
		want *regexp.Regexp
	}{
		"--pod-safety": {[]string{"--pod-safety"}, regexp.MustCompile(`^/\S+/podsafety-overlay\.yml:/etc/legion-operator/overlay\.yml written true$`)},
		"bare":         {nil, regexp.MustCompile(`^/etc/legion-operator/overlay\.yml absent unset$`)},
	} {
		t.Run(name, func(t *testing.T) {
			seen := filepath.Join(t.TempDir(), "seen")
			t.Setenv("LEGION_TEST_SEEN", seen)
			code, stdout, stderr := probeImage(tc.args...)
			if code != 0 || !strings.HasPrefix(stdout, "probe-image: OK (") {
				t.Fatalf("probe-image %v = %d %q %q, want the OK line", tc.args, code, stdout, stderr)
			}
			raw, err := os.ReadFile(seen)
			if err != nil {
				t.Fatal(err)
			}
			runs := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(runs) < 3 {
				t.Fatalf("Oh My Pi ran %d times, want the three probes: %q", len(runs), runs)
			}
			for _, run := range runs {
				if !tc.want.MatchString(run) {
					t.Errorf("a probe ran Oh My Pi with %q, want %s", run, tc.want)
				}
			}
		})
	}
}

func TestProbeImageRefusesWithoutAnOmpOrAContract(t *testing.T) {
	inImage(t, thisBinarysContract, "")
	code, _, stderr := probeImage()
	if code != 1 || !strings.Contains(stderr, "set LEGION_OMP_PATH (or pass --omp) to the OMP executable to probe") {
		t.Errorf("probe-image with no OMP = %d %q, want exit 1 naming LEGION_OMP_PATH and --omp", code, stderr)
	}

	t.Setenv("LEGION_OMP_PATH", imageOmp(t))
	for _, value := range []string{"0", "-1", "3x", "", "99999999999999999999"} {
		code, stdout, stderr := probeImage("--go-daemon-api-version", value)
		if code != 1 || stdout != "" || !strings.Contains(stderr, `--go-daemon-api-version must be a positive integer (got "`+value+`")`) {
			t.Errorf("probe-image --go-daemon-api-version %q = %d %q %q, want exit 1 refusing the value", value, code, stdout, stderr)
		}
	}
}
