package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// imageOmp is an `omp` that passes the image's three probes as a working image's Oh My Pi does:
// pi.agents is there, the Envoy and Legion plugins handed to it as its two explicit extensions
// beside its discovery load from those roots, pi-envoy publishing the interface pi-legion speaks
// from the one module instance, and Oh My Pi finds every task agent and skill Legion's prompts
// name; and the session-storage setting refuses a value it does not know, naming the variable.
// Asked for task agents, it resolves their models too, unless told to skip them. With
// $LEGION_TEST_SEEN set, each run appends the environment it saw there: PI_CONFIG_FILES, whether
// the first overlay it names exists, and OMP_SESSION_STORAGE; with $LEGION_TEST_KEY_SEEN, the
// provider key TEST_PROVIDER_KEY.
func imageOmp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp")
	script := `#!/bin/sh
if [ -n "${LEGION_TEST_SEEN:-}" ]; then
  first=${PI_CONFIG_FILES%%:*}
  if [ -n "$first" ] && [ -f "$first" ]; then written=written; else written=absent; fi
  printf '%s %s %s\n' "${PI_CONFIG_FILES:-none}" "$written" "${OMP_SESSION_STORAGE:-unset}" >>"$LEGION_TEST_SEEN"
fi
[ -z "${LEGION_TEST_KEY_SEEN:-}" ] || printf '%s\n' "${TEST_PROVIDER_KEY:-unset}" >>"$LEGION_TEST_KEY_SEEN"
case "$*" in
"models --extension "*" --extension "*" --extension "*" --json")
  envoy=$(cd "$3" && pwd -P)
  root=$(cd "$5" && pwd -P)
  printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/dist/legion.js\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n' "$root" >&2
  printf 'LEGION_ENVOY_INTERFACE=1\nLEGION_ENVOY_LOADED_FROM=file://%s/dist/envoy.js\nLEGION_ENVOY_PUBLISHERS=1\nLEGION_ENVOY_PUBLISHER=file://%s/dist/envoy.js\n' "$envoy" "$envoy" >&2
  if [ -n "${LEGION_PROMPT_AGENTS:-}" ]; then echo LEGION_PROMPT_AGENTS=resolved >&2; fi
  if [ -n "${LEGION_PROMPT_AGENTS:-}" ] && [ -z "${LEGION_SKIP_AGENT_MODELS:-}" ]; then echo LEGION_AGENT_MODELS=resolved >&2; fi
  if [ -n "${LEGION_PROMPT_SKILLS:-}" ]; then echo LEGION_PROMPT_SKILLS=resolved >&2; fi ;;
"models --no-extensions --extension "*" --json") echo LEGION_OMP_AGENTS=available >&2 ;;
*) echo "Invalid $OMP_SESSION_STORAGE for OMP_SESSION_STORAGE" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	return path
}

// thisBinarysContract is the daemon API contract this binary speaks, as a manifest writes it.
var thisBinarysContract = strconv.Itoa(api.DaemonAPIVersion)

// image is the worker image's two plugin roots, which every probe-image run is given.
type image struct{ legion, envoy string }

// flags are the run's two root flags, then extra.
func (i image) flags(extra ...string) []string {
	return append([]string{"--plugin-root", i.legion, "--envoy-plugin-root", i.envoy}, extra...)
}

// inImage sets this process's environment to the worker image's: a HOME whose `legion` profile
// links neither plugin (a pod names both as explicit extensions, and a linked one would load
// twice), the Legion manifest declaring contract, and LEGION_OMP_PATH set to omp. It answers the
// plugin roots a pod loads.
func inImage(t *testing.T, contract, omp string) image {
	t.Helper()
	home := t.TempDir()
	roots := image{legion: filepath.Join(home, "pi-legion"), envoy: filepath.Join(home, "pi-envoy")}
	for _, plugin := range []struct{ root, manifest string }{
		{roots.legion, `{"name":"@sjawhar/pi-legion","version":"1.57.0","legion":{"daemonApiVersion":` + contract + `}}`},
		{roots.envoy, `{"name":"@sjawhar/pi-envoy","version":"1.57.0"}`},
	} {
		if err := os.MkdirAll(plugin.root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin.root, "package.json"), []byte(plugin.manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(home, ".omp", "profiles", "legion", "plugins", "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("OMP_PROFILE", "legion")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("LEGION_OMP_PATH", omp)
	t.Chdir(t.TempDir())
	return roots
}

func probeImage(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "probe-image"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// As the worker image's build runs it, with no contract named, the command holds the image's plugin
// to the contract this binary speaks and prints the OK line the daemon's probe Sandbox reads.
func TestProbeImagePrintsTheOKLineWithThisBinarysContract(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)

	code, stdout, stderr := probeImage(img.flags()...)

	if code != 0 {
		t.Fatalf("probe-image exited %d: %s", code, stderr)
	}
	if want := "probe-image: OK (" + omp + ") session-storage=probed extensions=discovered agent-models=resolved daemon-api-version=" + thisBinarysContract + "\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
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

	if code != 0 || stdout != "probe-image: OK ("+omp+") session-storage=probed extensions=discovered agent-models=resolved daemon-api-version="+thisBinarysContract+"\n" {
		t.Fatalf("probe-image --omp = %d %q %q, want the OK line naming %s", code, stdout, stderr, omp)
	}
}

// The image build's probe, which has none of the operator's model configuration, skips the agents'
// models, and its OK line says so, which the daemon's probe Sandbox refuses at boot.
func TestProbeImageWithSkipAgentModelsSaysSoOnTheOKLine(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)

	code, stdout, stderr := probeImage(img.flags("--skip-agent-models")...)

	if want := "probe-image: OK (" + omp + ") session-storage=probed extensions=discovered agent-models=skipped daemon-api-version=" + thisBinarysContract + "\n"; code != 0 || stdout != want {
		t.Fatalf("probe-image --skip-agent-models = %d %q %q, want %q", code, stdout, stderr, want)
	}
}

// A probe pod's NATS_NKEY_SEED_FILE names the providers Secret's seed: the command names that seed's
// user, by its public key alone, on the line before the OK line, for the daemon to compare with its
// own; a seed that is not a user's is refused before any probe runs. Neither output carries a seed.
func TestProbeImageNamesTheUserOfTheSeedItsPointerNames(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	seed, public := testnats.User(t)
	t.Setenv("NATS_NKEY_SEED_FILE", testnats.SeedFile(t, seed+"\n"))

	code, stdout, stderr := probeImage(img.flags()...)

	want := "probe-image: nats-nkey-user=" + public + "\nprobe-image: OK (" + omp + ") session-storage=probed extensions=discovered agent-models=resolved daemon-api-version=" + thisBinarysContract + "\n"
	if code != 0 || stdout != want || strings.Contains(stdout+stderr, seed) {
		t.Fatalf("probe-image with a user seed = %d %q %q, want %q and no seed", code, stdout, stderr, want)
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
	code, stdout, stderr = probeImage(img.flags("--provider-env-dir", providers)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "TEST_PROVIDER_KEY") {
		t.Fatalf("probe-image --provider-env-dir over a set TEST_PROVIDER_KEY = %d %q %q, want exit 1 naming the key", code, stdout, stderr)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Errorf("Oh My Pi ran before the refusal (%v)", err)
	}
}

// With --pod-safety, as the daemon's probe Sandbox runs it, every probe runs Oh My Pi on the pod's
// baseline, as a pod's shim starts it: the turn-scoping overlay written and named first in
// PI_CONFIG_FILES, ahead of the pod's own, and OMP_SESSION_STORAGE=file where the pod leaves it
// unset — except on the session-storage probe, whose own export of that variable outranks the
// baseline as a pod's own value does (bootgate.verifySessionStorage). Without it, as the image's
// build runs it, the probes run on the environment as it is: the operator's overlay alone, which
// nothing wrote, and the variable unset.
func TestProbeImageWithPodSafetyProbesOnThePodsBaseline(t *testing.T) {
	omp := imageOmp(t)
	img := inImage(t, thisBinarysContract, omp)
	// The operator's overlay, at a path nothing on this machine holds, so "absent" is the probe's
	// doing and not the machine's.
	operator := filepath.Join(t.TempDir(), "operator.yml")
	t.Setenv("PI_CONFIG_FILES", operator)
	t.Setenv("OMP_SESSION_STORAGE", "")
	os.Unsetenv("OMP_SESSION_STORAGE")
	for name, tc := range map[string]struct {
		args     []string
		overlays *regexp.Regexp
		sessions string
	}{
		"--pod-safety": {img.flags("--pod-safety"), regexp.MustCompile(`^/\S+/` + regexp.QuoteMeta(podsafety.TurnScopeFile+":"+operator) + ` written$`), "file"},
		"bare":         {img.flags(), regexp.MustCompile(`^` + regexp.QuoteMeta(operator) + ` absent$`), "unset"},
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
			sessions := map[string]int{}
			for _, run := range runs {
				fields := strings.Fields(run)
				if len(fields) != 3 || !tc.overlays.MatchString(fields[0]+" "+fields[1]) {
					t.Errorf("a probe ran Oh My Pi with %q, want %s", run, tc.overlays)
					continue
				}
				sessions[fields[2]]++
			}
			if want := map[string]int{tc.sessions: len(runs) - 1, "legion-launch-probe": 1}; !maps.Equal(sessions, want) {
				t.Errorf("the probes ran Oh My Pi with OMP_SESSION_STORAGE %v, want %v: the session-storage probe's own value once, %q otherwise", sessions, want, tc.sessions)
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
