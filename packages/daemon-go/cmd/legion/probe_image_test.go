package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// imageOmp is an `omp` that passes the image's three probes as a working image's Oh My Pi does:
// pi.agents is there, the plugin linked into the profile loads from its own package, and the
// session-storage setting refuses a value it does not know, naming the variable. Its model round
// trip answers from the profile's default alias when the profile's models.yml routes anthropic to
// the gateway $LEGION_TEST_ROUTE names, and refuses as Oh My Pi does without a usable model
// otherwise.
func imageOmp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp")
	script := `#!/bin/sh
installed=$(cd "$HOME/.omp/profiles/$OMP_PROFILE/plugins/node_modules/@sjawhar/pi-legion-envoy" && pwd -P)
case "$*" in
"models --no-extensions --extension "*" --json") echo LEGION_OMP_AGENTS=available >&2 ;;
"models --extension "*" --json") printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/dist/legion.js\n' "$installed" >&2 ;;
"-p --mode json "*)
  if ! grep -qx "    baseUrl: $LEGION_TEST_ROUTE" "$HOME/.omp/profiles/$OMP_PROFILE/agent/models.yml" 2>/dev/null; then
    echo "No model available matching enabledModels (anthropic/*-legion) with usable credentials." >&2; exit 1
  fi
  printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"provider":"anthropic","model":"claude-fable-5-1-legion","stopReason":"stop"}}' ;;
*) echo "Invalid $OMP_SESSION_STORAGE for OMP_SESSION_STORAGE" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	return path
}

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
	inImage(t, "3", omp)

	code, stdout, stderr := probeImage()

	if code != 0 {
		t.Fatalf("probe-image exited %d: %s", code, stderr)
	}
	if want := "probe-image: OK (" + omp + ") session-storage=probed go-daemon-api-version=3\n"; stdout != want {
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
	inImage(t, "3", "/nonexistent/omp")

	code, stdout, stderr := probeImage("--omp", omp)

	if code != 0 || stdout != "probe-image: OK ("+omp+") session-storage=probed go-daemon-api-version=3\n" {
		t.Fatalf("probe-image --omp = %d %q %q, want the OK line naming %s", code, stdout, stderr, omp)
	}
}

// In a pod the daemon routed through the model gateway, the command writes the route into the
// image's `legion` profile before it probes, makes the round trip through it, and names the model
// that answered on the OK line, which the daemon requires. A gateway it cannot route is refused
// naming the variable, and so is a round trip the profile cannot make, with no OK line either way.
func TestProbeImageMakesTheRoundTripThroughTheGateway(t *testing.T) {
	omp := imageOmp(t)
	inImage(t, "3", omp)
	t.Setenv("LEGION_MODEL_GATEWAY_URL", "https://middleman.legion.internal")
	t.Setenv("LEGION_TEST_ROUTE", "https://middleman.legion.internal/anthropic")

	code, stdout, stderr := probeImage("--go-daemon-api-version", "3")

	want := "probe-image: OK (" + omp + ") session-storage=probed model-gateway=anthropic/claude-fable-5-1-legion go-daemon-api-version=3\n"
	if code != 0 || stdout != want {
		t.Fatalf("probe-image in a routed pod = %d %q %q, want the OK line naming the model", code, stdout, stderr)
	}

	t.Setenv("LEGION_TEST_ROUTE", "https://elsewhere.internal/anthropic")
	code, stdout, stderr = probeImage("--go-daemon-api-version", "3")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "routed to https://middleman.legion.internal/anthropic, found no usable model") {
		t.Errorf("probe-image whose round trip finds no model = %d %q %q, want exit 1 naming the route", code, stdout, stderr)
	}

	t.Setenv("LEGION_MODEL_GATEWAY_URL", "ftp://middleman.legion.internal")
	code, stdout, stderr = probeImage("--go-daemon-api-version", "3")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "LEGION_MODEL_GATEWAY_URL") {
		t.Errorf("probe-image with an unroutable gateway = %d %q %q, want exit 1 naming the variable", code, stdout, stderr)
	}
}

func TestProbeImageRefusesWithoutAnOmpOrAContract(t *testing.T) {
	inImage(t, "3", "")
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
