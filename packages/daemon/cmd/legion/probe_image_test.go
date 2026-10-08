package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// imageOmp is an `omp` that passes the image's three probes as a working image's Oh My Pi does:
// pi.agents is there, the Envoy and Legion plugins handed to it as its two explicit extensions
// load from those roots, pi-envoy publishing the interface pi-legion speaks, and Oh My Pi finds
// every task agent and skill Legion's prompts name; and the session-storage setting refuses a
// value it does not know, naming the variable. Asked for task agents, it resolves their models
// too, unless told to skip them. It answers the capability check's two commands as this pod's omp
// does: `setup python --check --json` says Python is available when python3 is on its PATH and
// $LEGION_TEST_NO_PYTHON is unset, else `available: false` and exit 1; `config get
// retry.modelFallback --json` says the setting is false, or true under $LEGION_TEST_FALLBACK_ON.
// With $LEGION_TEST_SEEN set, each run appends the environment it
// saw there: PI_CONFIG_FILES, whether the first overlay it names exists, and OTEL_SDK_DISABLED;
// with $LEGION_TEST_KEY_SEEN, the provider key TEST_PROVIDER_KEY.
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
case "$*" in
"models --no-extensions --extension "*" --extension "*" --extension "*" --json")
  envoy=$(cd "$4" && pwd -P)
  root=$(cd "$6" && pwd -P)
  printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/dist/legion.js\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n' "$root" >&2
  printf 'LEGION_ENVOY_INTERFACE=1\nLEGION_ENVOY_LOADED_FROM=file://%s/dist/envoy.js\n' "$envoy" >&2
  if [ -n "${LEGION_PROMPT_AGENTS:-}" ]; then echo LEGION_PROMPT_AGENTS=resolved >&2; fi
  if [ -n "${LEGION_PROMPT_AGENTS:-}" ] && [ -z "${LEGION_SKIP_AGENT_MODELS:-}" ]; then echo LEGION_AGENT_MODELS=resolved >&2; fi
  if [ -n "${LEGION_PROMPT_SKILLS:-}" ]; then echo LEGION_PROMPT_SKILLS=resolved >&2; fi ;;
"models --no-extensions --extension "*" --json") echo LEGION_OMP_AGENTS=available >&2 ;;
"setup python --check --json")
  if [ -n "${LEGION_TEST_NO_PYTHON:-}" ] || ! command -v python3 >/dev/null 2>&1; then
    echo '{"available": false, "usingManagedEnv": false, "managedEnvPath": ""}'; exit 1
  fi
  echo '{"available": true}' ;;
"config get retry.modelFallback --json")
  if [ -n "${LEGION_TEST_FALLBACK_ON:-}" ]; then value=true; else value=false; fi
  printf '{"key":"retry.modelFallback","value":%s,"type":"boolean"}\n' "$value" ;;
*) echo "Invalid $OMP_SESSION_STORAGE for OMP_SESSION_STORAGE" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	return path
}

// imageBinaries are the binaries the capability check's image rows look for, each stubbed on the
// test's PATH: `go version` answers a Go version, `chromium --version` a Chromium one.
var imageBinaries = []string{"gopls", "typescript-language-server", "pyright-langserver", "codegraph", "go", "curl", "wget", "python3", "node", "bun", "uv", "chromium"}

const imageBinary = `#!/bin/sh
case "$1" in
version) echo "go version go1.26.8 linux/amd64" ;;
--version) echo "Chromium 141.0.0.0" ;;
esac
`

// codeGraphLock is the plugin lock `omp plugin install` writes in the worker image, with the
// CodeGraph plugin enabled.
const codeGraphLock = `{"plugins":{"@bopstack/pi-codegraph":{"version":"0.1.1","enabledFeatures":null,"enabled":true}}}`

// thisBinarysContract is the daemon API contract this binary speaks, as a manifest writes it.
var thisBinarysContract = strconv.Itoa(api.DaemonAPIVersion)

// image is the worker image's two plugin roots, which every probe-image run is given, its bin of
// stubbed binaries, first on PATH, and its profile's plugin lock.
type image struct{ legion, envoy, bin, lock string }

// flags are the run's two root flags, then extra.
func (i image) flags(extra ...string) []string {
	return append([]string{"--plugin-root", i.legion, "--envoy-plugin-root", i.envoy}, extra...)
}

// without removes a stubbed binary from the image's PATH.
func (i image) without(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(i.bin, name)); err != nil {
		t.Fatal(err)
	}
}

// inImage sets this process's environment to the worker image's: a HOME whose `legion` profile
// links both plugins and whose plugin lock enables the CodeGraph plugin, the Legion manifest
// declaring contract, LEGION_OMP_PATH set to omp, and a bin first on PATH stubbing every binary
// the capability check's image rows look for. It answers the plugin roots a pod loads.
func inImage(t *testing.T, contract, omp string) image {
	t.Helper()
	home := t.TempDir()
	roots := image{
		legion: filepath.Join(home, "pi-legion"), envoy: filepath.Join(home, "pi-envoy"), bin: t.TempDir(),
		lock: filepath.Join(home, ".omp", "profiles", "legion", "plugins", "omp-plugins.lock.json"),
	}
	for _, plugin := range []struct{ name, root, manifest string }{
		{"pi-legion", roots.legion, `{"name":"@sjawhar/pi-legion","version":"1.57.0","legion":{"daemonApiVersion":` + contract + `}}`},
		{"pi-envoy", roots.envoy, `{"name":"@sjawhar/pi-envoy","version":"1.57.0"}`},
	} {
		if err := os.MkdirAll(plugin.root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin.root, "package.json"), []byte(plugin.manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		installed := filepath.Join(home, ".omp", "profiles", "legion", "plugins", "node_modules", "@sjawhar", plugin.name)
		if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(plugin.root, installed); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(roots.lock, []byte(codeGraphLock), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range imageBinaries {
		if err := os.WriteFile(filepath.Join(roots.bin, name), []byte(imageBinary), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("OMP_PROFILE", "legion")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LEGION_OMP_PATH", omp)
	t.Setenv("PATH", roots.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Chdir(t.TempDir())
	return roots
}

// capabilityMarks are the OK line's marks after agent-models under a stubbed image whose omp
// says model fallback is off.
const capabilityMarks = " capabilities=checked model-fallback=off daemon-api-version="

func probeImage(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "probe-image"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// tableLines are stdout's lines before the NATS user line and the OK line: the capability table.
// Each is one row of capabilities.Table, rendered by its Line, so stdout is split on the row shape.
func tableLines(stdout string) []string {
	var lines []string
	for line := range strings.Lines(stdout) {
		if strings.HasPrefix(line, "probe-image: capability ") {
			lines = append(lines, strings.TrimSuffix(line, "\n"))
		}
	}
	return lines
}

// As the worker image's build runs it, with no contract named, the command holds the image's plugin
// to the contract this binary speaks, prints the capability table — one line per row of the
// declared list, in its order, every image row present — and then the OK line the daemon's probe
// Sandbox reads, carrying the capabilities and model-fallback marks.
func TestProbeImagePrintsTheTableAndTheOKLineWithThisBinarysContract(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)

	code, stdout, stderr := probeImage(img.flags()...)

	if code != 0 {
		t.Fatalf("probe-image exited %d: %s", code, stderr)
	}
	table := tableLines(stdout)
	if len(table) != len(capabilities.Table) {
		t.Fatalf("stdout carries %d capability lines, want one per row (%d):\n%s", len(table), len(capabilities.Table), stdout)
	}
	for i, row := range capabilities.Table {
		if prefix := "probe-image: capability " + string(row.Name) + ": "; !strings.HasPrefix(table[i], prefix) {
			t.Errorf("capability line %d = %q, want row %s: the table's order", i, table[i], row.Name)
		}
		if row.Site == capabilities.SiteImage && !strings.HasPrefix(table[i], "probe-image: capability "+string(row.Name)+": present (") {
			t.Errorf("capability line %d = %q, want %s present", i, table[i], row.Name)
		}
	}
	okLine := "probe-image: OK (" + omp + ") session-storage=probed agent-models=resolved" + capabilityMarks + thisBinarysContract + "\n"
	if want := strings.Join(table, "\n") + "\n" + okLine; stdout != want {
		t.Fatalf("stdout = %q, want the table then the OK line %q", stdout, okLine)
	}
}

// The daemon's probe Sandbox passes its own contract, which the image's plugin must declare.
func TestProbeImageHoldsThePluginToTheContractItIsAskedFor(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, "2", omp)

	code, stdout, stderr := probeImage(img.flags("--daemon-api-version", "2")...)
	if code != 0 || !strings.HasSuffix(stdout, " daemon-api-version=2\n") {
		t.Fatalf("probe-image --daemon-api-version 2 over a plugin declaring 2 = %d %q %q, want the OK line confirming 2", code, stdout, stderr)
	}

	code, stdout, stderr = probeImage(img.flags("--daemon-api-version", "4")...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "speaks daemon API contract 2; this daemon requires 4") {
		t.Fatalf("probe-image --daemon-api-version 4 over a plugin declaring 2 = %d %q %q, want exit 1 naming both contracts and no OK line", code, stdout, stderr)
	}
}

func TestProbeImageProbesTheOmpItIsGiven(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, "/nonexistent/omp")

	code, stdout, stderr := probeImage(img.flags("--omp", omp)...)

	if code != 0 || !strings.HasSuffix(stdout, "\nprobe-image: OK ("+omp+") session-storage=probed agent-models=resolved"+capabilityMarks+thisBinarysContract+"\n") {
		t.Fatalf("probe-image --omp = %d %q %q, want the OK line naming %s", code, stdout, stderr, omp)
	}
}

// The image build's probe, which has none of the operator's model configuration, skips the agents'
// models, and its OK line says so, which the daemon's probe Sandbox refuses at boot.
func TestProbeImageWithSkipAgentModelsSaysSoOnTheOKLine(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)

	code, stdout, stderr := probeImage(img.flags("--skip-agent-models")...)

	if want := "\nprobe-image: OK (" + omp + ") session-storage=probed agent-models=skipped" + capabilityMarks + thisBinarysContract + "\n"; code != 0 || !strings.HasSuffix(stdout, want) {
		t.Fatalf("probe-image --skip-agent-models = %d %q %q, want the OK line %q", code, stdout, stderr, want)
	}
}

// The OK line's model-fallback mark is what the image's Oh My Pi answers for retry.modelFallback
// under the probe's environment: off by default, on when the operator's configuration turns it on.
func TestProbeImageMarksTheOKLineWithTheModelFallbackItRead(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	for mark, on := range map[string]bool{"off": false, "on": true} {
		t.Run(mark, func(t *testing.T) {
			if on {
				t.Setenv("LEGION_TEST_FALLBACK_ON", "1")
			}

			code, stdout, stderr := probeImage(img.flags()...)

			if want := " capabilities=checked model-fallback=" + mark + " daemon-api-version=" + thisBinarysContract + "\n"; code != 0 || !strings.HasSuffix(stdout, want) {
				t.Fatalf("probe-image = %d %q %q, want the OK line ending %q", code, stdout, stderr, want)
			}
		})
	}
}

// A capability the image lacks fails the probe after the launch probes passed: the whole table is
// printed, the rows that are present saying so, the missing row saying why; stderr names the
// missing capability; exit 1; no OK line. Each image row is checked where it is checked: Python by
// Oh My Pi's own answer — which, as in the image, says unavailable without a python3, and under
// $LEGION_TEST_NO_PYTHON says so with python3 on PATH, so the row is the answer, not the PATH —
// the browser by running what PUPPETEER_EXECUTABLE_PATH names, as Oh My Pi would, CodeGraph by
// the profile's plugin lock, the toolchain by PATH.
func TestProbeImageRefusesAnImageMissingACapability(t *testing.T) {
	for name, testCase := range map[string]struct {
		setup   func(t *testing.T, img image)
		missing []string
	}{
		"python3 off PATH":                         {func(t *testing.T, img image) { img.without(t, "python3") }, []string{"eval-python", "toolchain"}},
		"Oh My Pi's Python unavailable":            {func(t *testing.T, img image) { t.Setenv("LEGION_TEST_NO_PYTHON", "1") }, []string{"eval-python"}},
		"PUPPETEER_EXECUTABLE_PATH naming nothing": {func(t *testing.T, img image) { t.Setenv("PUPPETEER_EXECUTABLE_PATH", "/nonexistent/chromium") }, []string{"browser"}},
		"the CodeGraph plugin not in the lock": {func(t *testing.T, img image) {
			if err := os.WriteFile(img.lock, []byte(`{"plugins":{"@sjawhar/pi-legion":{"enabled":true}}}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"codegraph"}},
		"gopls off PATH": {func(t *testing.T, img image) { img.without(t, "gopls") }, []string{"lsp"}},
	} {
		t.Run(name, func(t *testing.T) {
			omp := imageOmp(t)
			img := inImage(t, thisBinarysContract, omp)
			testCase.setup(t, img)

			code, stdout, stderr := probeImage(img.flags()...)

			if code != 1 || strings.Contains(stdout, "probe-image: OK") {
				t.Fatalf("probe-image = %d %q %q, want exit 1 and no OK line", code, stdout, stderr)
			}
			for _, missing := range testCase.missing {
				if !strings.Contains(stderr, "capability "+missing+" is missing: ") {
					t.Errorf("stderr = %q, want it to name capability %s missing", stderr, missing)
				}
				if !strings.Contains(stdout, "probe-image: capability "+missing+": missing (") {
					t.Errorf("stdout = %q, want the %s row saying missing", stdout, missing)
				}
			}
			if len(tableLines(stdout)) != len(capabilities.Table) {
				t.Errorf("stdout carries %d capability lines, want the whole table (%d) however many rows are missing", len(tableLines(stdout)), len(capabilities.Table))
			}
			for _, row := range capabilities.Table {
				if row.Site == capabilities.SiteImage && !slices.Contains(testCase.missing, string(row.Name)) && !strings.Contains(stdout, "probe-image: capability "+string(row.Name)+": present (") {
					t.Errorf("stdout = %q, want the %s row still present", stdout, row.Name)
				}
			}
		})
	}
}

// A probe pod's NATS_NKEY_SEED_FILE names the providers Secret's seed: the command names that seed's
// user, by its public key alone, on the line before the OK line and after the capability table,
// for the daemon to compare with its own; a seed that is not a user's is refused before any probe
// runs. Neither output carries a seed.
func TestProbeImageNamesTheUserOfTheSeedItsPointerNames(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	seed, public := testnats.User(t)
	t.Setenv("NATS_NKEY_SEED_FILE", testnats.SeedFile(t, seed+"\n"))

	code, stdout, stderr := probeImage(img.flags()...)

	want := ")\nprobe-image: nats-nkey-user=" + public + "\nprobe-image: OK (" + omp + ") session-storage=probed agent-models=resolved" + capabilityMarks + thisBinarysContract + "\n"
	if code != 0 || !strings.HasSuffix(stdout, want) || strings.Contains(stdout+stderr, seed) {
		t.Fatalf("probe-image with a user seed = %d %q %q, want the table, then %q, and no seed", code, stdout, stderr, want)
	}

	account := testnats.Account(t)
	file := testnats.SeedFile(t, account)
	t.Setenv("NATS_NKEY_SEED_FILE", file)
	code, stdout, stderr = probeImage(img.flags()...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "NATS_NKEY_SEED_FILE ("+file+") holds an nkey seed that is not a user's") || strings.Contains(stderr, account) {
		t.Fatalf("probe-image with an account seed = %d %q %q, want exit 1 naming the pointer, no OK line and no seed", code, stdout, stderr)
	}
}

// The pointer is read as the daemon reads its seed (natsauth.Seed): the pod's mount, root's and
// 0440 under fsGroup, is read through the group (config.ReadGroupSecretPointer's test; a test
// cannot make a file root's), and a file the probe's own uid owns is held to 0600.
func TestProbeImageReadsTheSeedPointerByTheDaemonsModeRule(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	seed, _ := testnats.User(t)
	file := testnats.SeedFile(t, seed+"\n")
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NATS_NKEY_SEED_FILE", file)

	code, stdout, stderr := probeImage(img.flags()...)

	if want := "NATS_NKEY_SEED_FILE " + file + " is readable by its group or others (mode 0640); chmod 0600 it"; code != 1 || stdout != "" || !strings.Contains(stderr, want) || strings.Contains(stderr, seed) {
		t.Fatalf("probe-image with a 0640 seed it owns = %d %q %q, want exit 1 saying %q and no seed", code, stdout, stderr, want)
	}
}

// With --provider-env-dir, as the probe Sandbox runs it when provider keys are configured, every
// probe's Oh My Pi gets each key as a worker's shim exports it, so an agent keyed only through the
// providers Secret resolves as it would in a worker; a key the environment already names is refused
// as the shim refuses it, before any probe runs.
func TestProbeImageWithAProviderEnvDirExportsTheKeysAsTheShimDoes(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	providers := t.TempDir()
	if err := os.WriteFile(filepath.Join(providers, "TEST_PROVIDER_KEY"), []byte("from-the-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(t.TempDir(), "seen")
	t.Setenv("LEGION_TEST_KEY_SEEN", seen)

	code, stdout, stderr := probeImage(img.flags("--provider-env-dir", providers)...)

	if code != 0 || !strings.Contains(stdout, "\nprobe-image: OK (") {
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
	code, stdout, stderr = probeImage(img.flags("--provider-env-dir", providers)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "TEST_PROVIDER_KEY") {
		t.Fatalf("probe-image --provider-env-dir over a set TEST_PROVIDER_KEY = %d %q %q, want exit 1 naming the key", code, stdout, stderr)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("Oh My Pi ran before the refusal (%v)", err)
	}
}

// With --pod-safety, as the daemon's probe Sandbox runs it, every probe runs Oh My Pi on the pod's
// baseline, as a pod's shim starts it: the overlay written and named first in PI_CONFIG_FILES,
// ahead of the pod's own, and OpenTelemetry held off. Without it, as the image's build runs it, the
// probes run on the environment as it is.
func TestProbeImageWithPodSafetyProbesOnThePodsBaseline(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	t.Setenv("PI_CONFIG_FILES", "/etc/legion-operator/overlay.yml")
	t.Setenv("OTEL_SDK_DISABLED", "")
	for name, tc := range map[string]struct {
		args []string
		want *regexp.Regexp
	}{
		"--pod-safety": {img.flags("--pod-safety"), regexp.MustCompile(`^/\S+/podsafety-overlay\.yml:/etc/legion-operator/overlay\.yml written true$`)},
		"bare":         {img.flags(), regexp.MustCompile(`^/etc/legion-operator/overlay\.yml absent unset$`)},
	} {
		t.Run(name, func(t *testing.T) {
			seen := filepath.Join(t.TempDir(), "seen")
			t.Setenv("LEGION_TEST_SEEN", seen)
			code, stdout, stderr := probeImage(tc.args...)
			if code != 0 || !strings.Contains(stdout, "\nprobe-image: OK (") {
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

// A --role-references value that is not promptrefs.Encode's encoding is refused as the flags are
// read, before any probe runs: resolving the image's own role prompts in its place would pass a
// probe the daemon asked about its own.
func TestProbeImageRefusesRoleReferencesItCannotDecode(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	seen := filepath.Join(t.TempDir(), "seen")
	t.Setenv("LEGION_TEST_SEEN", seen)

	code, stdout, stderr := probeImage(img.flags("--role-references",
		`{"LEGION_PROMPT_AGENTS":{},"LEGION_PROMPT_SKILLS":{},"LEGION_PROMPT_AGENTS":{}}`)...)

	if code != 1 || stdout != "" || !strings.Contains(stderr, "--role-references") || !strings.Contains(stderr, "appears twice") {
		t.Fatalf("probe-image with a repeated kind in --role-references = %d %q %q, want exit 1 naming the flag and the repeated key, and no OK line", code, stdout, stderr)
	}
	if _, err := os.Stat(seen); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Oh My Pi ran (%s: %v), want no probe", seen, err)
	}
}

// Every caller loads the plugins as a pod does, from their roots: the worker image's build and the
// daemon's probe Sandbox. A run without --plugin-root or --envoy-plugin-root is a usage error
// naming the flag, so deleting either from the image's build line fails that build rather than
// probing a lane no pod loads.
func TestProbeImageRefusesWithoutAPluginRoot(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	for _, testCase := range []struct {
		name string
		args []string
		want string
	}{
		{"no roots", nil, "legion probe-image: --plugin-root is required"},
		{"no Envoy plugin root", []string{"--plugin-root", img.legion}, "legion probe-image: --envoy-plugin-root is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			seen := filepath.Join(t.TempDir(), "seen")
			t.Setenv("LEGION_TEST_SEEN", seen)

			code, stdout, stderr := probeImage(testCase.args...)

			if code != 2 || stdout != "" || !strings.Contains(stderr, testCase.want) {
				t.Fatalf("probe-image %v = %d %q %q, want exit 2 saying %q", testCase.args, code, stdout, stderr, testCase.want)
			}
			if _, err := os.Stat(seen); !os.IsNotExist(err) {
				t.Errorf("Oh My Pi ran before the refusal (%v)", err)
			}
		})
	}
}

func TestProbeImageRefusesWithoutAnOmpOrAContract(t *testing.T) {
	img := inImage(t, thisBinarysContract, "")
	code, _, stderr := probeImage(img.flags()...)
	if code != 1 || !strings.Contains(stderr, "set LEGION_OMP_PATH (or pass --omp) to the OMP executable to probe") {
		t.Errorf("probe-image with no OMP = %d %q, want exit 1 naming LEGION_OMP_PATH and --omp", code, stderr)
	}

	t.Setenv("LEGION_OMP_PATH", imageOmp(t))
	for _, value := range []string{"0", "-1", "3x", "", "99999999999999999999"} {
		code, stdout, stderr := probeImage(img.flags("--daemon-api-version", value)...)
		if code != 1 || stdout != "" || !strings.Contains(stderr, `--daemon-api-version must be a positive integer (got "`+value+`")`) {
			t.Errorf("probe-image --daemon-api-version %q = %d %q %q, want exit 1 refusing the value", value, code, stdout, stderr)
		}
	}
}
