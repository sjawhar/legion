package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// Under runtime tmux, `legion start --check-config` refuses, with exit 1 and no Config OK, a file
// role_config_files names that a pane of the role could not read, naming the role and the file, as
// boot does before any pane starts; a file that is there passes.
func TestStartCheckConfigRefusesARoleOverlayThePaneCannotRead(t *testing.T) {
	legionState(t)
	dir := t.TempDir()
	present, missing := filepath.Join(dir, "fallback.yml"), filepath.Join(dir, "missing.yml")
	if err := os.WriteFile(present, []byte("retry:\n  modelFallback: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config, _ := workflowConfig(t, 13370, "role_config_files: {implementer: ["+present+"]}\n")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	if want := "Config OK: project=DEMO\n" + tmuxCapabilityGaps; code != 0 || out.String() != want || errb.Len() != 0 {
		t.Fatalf("a present overlay: exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}

	config, _ = workflowConfig(t, 13370, "role_config_files: {implementer: ["+present+", "+missing+"]}\n")
	out.Reset()
	errb.Reset()
	code = run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	want := "legion start: role_config_files.implementer names " + missing + ", which the implementer's Oh My Pi could not read: stat " + missing + ": no such file or directory\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("a missing overlay: exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// Under runtime kubernetes, `legion start --check-config` refuses a role's overlay no mount of the
// operator's pod block holds, since no pod could read it, and passes one a mount holds; whether the
// ConfigMap holds the key is the image probe's to read.
func TestStartCheckConfigRefusesARoleOverlayNoPodMountHolds(t *testing.T) {
	legionState(t)
	const pod = "    pod:\n" +
		"      volumes:\n" +
		"        - {name: operator-route, config_map: {name: legion-operator-route}}\n" +
		"      volume_mounts:\n" +
		"        - {volume: operator-route, mount_path: /etc/legion-operator}\n"
	const secretsGap = "capability secrets is open: runtime.kubernetes.agent_secrets is not configured; to record a decision, add to legion.yaml: capabilities.decided.secrets: \"<reason>\"\n"
	const pluginsGap = "plugins: none — set runtime.kubernetes.plugins or run a release build; a daemon that knows none relaunches no process on another plugin contract\n"

	config := kubernetesCheckConfig(t, pod+"role_config_files: {implementer: [/etc/legion-operator/fallback.yml], merger: [/etc/legion-operator/fallback.yml]}\n")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	if want := "Config OK: project=DEMO\n" + secretsGap + defaultCacheLine + pluginsGap; code != 0 || out.String() != want || errb.Len() != 0 {
		t.Fatalf("a mounted overlay: exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
	}

	config = kubernetesCheckConfig(t, pod+"role_config_files: {tester: [/etc/legion-fallback/fallback.yml]}\n")
	out.Reset()
	errb.Reset()
	code = run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
	want := "legion start: role_config_files.tester entry /etc/legion-fallback/fallback.yml is under no runtime.kubernetes.pod.volume_mounts mount_path: a role's pod and the image probe's see only the image and the operator's mounts, so mount the ConfigMap that holds it there\n"
	if code != 1 || out.Len() != 0 || errb.String() != want {
		t.Fatalf("an unmounted overlay: exit code = %d, stdout %q, stderr %q; want 1, nothing, and %q", code, out.String(), errb.String(), want)
	}
}

// A role whose check timed out on one run and refused on a later one is refused: the timeout is
// printed, then the refusal line the daemon reads, and the exit is 1; a role that only timed out
// passes with the timeout printed.
func TestProbeImageRefusesARoleOverlayAfterATimeout(t *testing.T) {
	timedOut := errors.New("omp config get retry.modelFallback --json under PI_CONFIG_FILES /a.yml: timed out after 1m0s")
	for _, tc := range []struct {
		name, why string
		code      int
	}{
		{"a refusal after the timeout", "Oh My Pi refuses /b.yml: error: Failed to parse config overlay /b.yml", 1},
		{"the timeout alone", "", 0},
	} {
		var stderr bytes.Buffer
		code := checkRoleOverlays([]roleOverlays{{role: "merger", files: []string{"/a.yml", "/b.yml"}}}, func([]string) (string, error) { return tc.why, timedOut }, &stderr)
		if code != tc.code || !strings.Contains(stderr.String(), "timed out after 1m0s") {
			t.Errorf("%s: exit %d, stderr %q; want %d and the timeout", tc.name, code, stderr.String(), tc.code)
		}
		if refused := strings.Contains(stderr.String(), bootprobe.RoleOverlaysRefusedLine("merger", tc.why)); refused != (tc.why != "") {
			t.Errorf("%s: stderr %q, refusal line printed %t", tc.name, stderr.String(), refused)
		}
	}
}
