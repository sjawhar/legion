package podsafety

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// The overlay holds off every setting a repository could name that sends a pod's conversation
// somewhere Oh My Pi posts to on its own, and leaves session storage to OMP_SESSION_STORAGE, which
// outranks the setting.
func TestThePinsHoldTheSelfPostingEndpointsOff(t *testing.T) {
	var pins map[string]any
	if err := yaml.Unmarshal(overlay, &pins); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]any{
		"compaction.remoteEndpoint": "",
		"memory.backend":            "off",
		"images.urls.enabled":       false,
		"dev.autoqa":                false,
	} {
		var got any = pins
		for _, key := range strings.Split(path, ".") {
			section, _ := got.(map[string]any)
			got = section[key]
		}
		if got != want {
			t.Errorf("the overlay holds %s = %#v, want %#v", path, got, want)
		}
	}
	if _, set := pins["session"]; set {
		t.Errorf("the overlay sets session %#v; OMP_SESSION_STORAGE outranks it, so the variable is the baseline's", pins["session"])
	}
}

// A bash call Oh My Pi leaves running past its own threshold, under `--mode rpc` (every Legion
// role), moves to a background job a takeover's abort no longer reaches once it has moved — only
// the turn asking for it to finish does. supervise.Machine.Quiesce's own promise, that the
// outgoing worker "never writes the shared workspace beside the role the start hands it to",
// depends on every long-running command staying inside the turn the abort ends (LEGION-462 found
// this live, legion-smoke LEGSMOKE-463, at commit 80d0c82b).
func TestThePinsHoldBashAutoBackgroundOff(t *testing.T) {
	var pins map[string]any
	if err := yaml.Unmarshal(overlay, &pins); err != nil {
		t.Fatal(err)
	}
	var got any = pins
	for _, key := range strings.Split("bash.autoBackground.enabled", ".") {
		section, _ := got.(map[string]any)
		got = section[key]
	}
	if got != false {
		t.Errorf("the overlay holds bash.autoBackground.enabled = %#v, want false", got)
	}
}

// With async on, the thermonuclear review pair a reviewer dispatches without `blocking: true` runs
// as background jobs a takeover's RPC abort does not own, and its async result starts a new
// reviewer turn beside whatever claims the issue next — a CI-red takeover mid-review among them,
// exactly the turn boundary Quiesce exists to hold (LEGION-462).
func TestThePinsHoldAsyncOff(t *testing.T) {
	var pins map[string]any
	if err := yaml.Unmarshal(overlay, &pins); err != nil {
		t.Fatal(err)
	}
	var got any = pins
	for _, key := range strings.Split("async.enabled", ".") {
		section, _ := got.(map[string]any)
		got = section[key]
	}
	if got != false {
		t.Errorf("the overlay holds async.enabled = %#v, want false", got)
	}
}

// TurnScopeOverlay is the two keys every Legion role's Oh My Pi needs regardless of runtime, for a
// runtime with no overlay mechanism of its own to write directly (runtime/tmux): both off there
// too, without the rest of the pod baseline.
func TestTheTurnScopeOverlayHoldsBothOff(t *testing.T) {
	var pins map[string]any
	if err := yaml.Unmarshal(TurnScopeOverlay, &pins); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"bash.autoBackground.enabled", "async.enabled"} {
		var got any = pins
		for _, key := range strings.Split(path, ".") {
			section, _ := got.(map[string]any)
			got = section[key]
		}
		if got != false {
			t.Errorf("the turn-scope overlay holds %s = %#v, want false", path, got)
		}
	}
}

func envMap(environ []string) map[string]string {
	env := map[string]string{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env
}

// named is how many times environ names variable.
func named(environ []string, variable string) int {
	return len(slices.DeleteFunc(slices.Clone(environ), func(pair string) bool { return !strings.HasPrefix(pair, variable+"=") }))
}

// Apply writes the overlay, read-only, under the state directory and names it first in
// PI_CONFIG_FILES, so every overlay the operator names after it outranks it, and every overlay
// outranks a repository's settings.
func TestApplyNamesTheOverlayFirstAheadOfTheOperators(t *testing.T) {
	for name, tc := range map[string]struct {
		operator []string
		want     string
	}{
		"the operator's overlays":  {[]string{"PI_CONFIG_FILES=/etc/op.yml:/etc/op2.yml"}, "%s:/etc/op.yml:/etc/op2.yml"},
		"no overlay of the pod's":  {nil, "%s"},
		"an empty PI_CONFIG_FILES": {[]string{"PI_CONFIG_FILES="}, "%s"},
	} {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			env, err := Apply(append([]string{"HOME=/home/legion"}, tc.operator...), state)
			if err != nil {
				t.Fatal(err)
			}
			written := filepath.Join(state, OverlayFile)
			if got, want := envMap(env)["PI_CONFIG_FILES"], strings.ReplaceAll(tc.want, "%s", written); got != want {
				t.Errorf("PI_CONFIG_FILES = %q, want %q", got, want)
			}
			if n := named(env, "PI_CONFIG_FILES"); n != 1 {
				t.Errorf("PI_CONFIG_FILES is named %d times in %q", n, env)
			}
			body, err := os.ReadFile(written)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(written)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != string(overlay) || info.Mode().Perm() != 0o444 {
				t.Errorf("%s holds %d bytes at mode %v, want the embedded overlay at 0444", written, len(body), info.Mode().Perm())
			}
		})
	}
}

// Each baseline variable is set only where the pod's environment leaves it unset or empty — the
// case in which Oh My Pi would fill it from a repository's .env — so the operator's own value wins,
// and so does the Sandbox runtime's session database, which it names before Apply runs.
func TestApplySetsEachBaselineVariableOnlyWhereThePodLeavesItUnset(t *testing.T) {
	baseline := map[string]string{"OTEL_SDK_DISABLED": "true", "PI_AUTO_QA": "0", "PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "file"}
	for name, tc := range map[string]struct {
		pod, want map[string]string
	}{
		"unset":                {map[string]string{}, baseline},
		"the operator's value": {map[string]string{"OTEL_SDK_DISABLED": "false", "OMP_SESSION_STORAGE": "sql"}, map[string]string{"OTEL_SDK_DISABLED": "false", "PI_AUTO_QA": "0", "PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "sql"}},
		"empty, as .env fills": {map[string]string{"PI_AUTO_QA": ""}, baseline},
		"another config root":  {map[string]string{"PI_CONFIG_DIR": ".config/omp"}, map[string]string{"OTEL_SDK_DISABLED": "true", "PI_AUTO_QA": "0", "PI_CONFIG_DIR": ".config/omp", "OMP_SESSION_STORAGE": "file"}},
		"the runtime's session database": {
			map[string]string{"OMP_SESSION_STORAGE": "sql", "OMP_SESSION_SQL_DSN_FILE": "/var/run/legion/providers/OMP_SESSION_SQL_DSN"},
			map[string]string{"OTEL_SDK_DISABLED": "true", "PI_AUTO_QA": "0", "PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "sql", "OMP_SESSION_SQL_DSN_FILE": "/var/run/legion/providers/OMP_SESSION_SQL_DSN"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var environ []string
			for variable, value := range tc.pod {
				environ = append(environ, variable+"="+value)
			}
			env, err := Apply(environ, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			got := envMap(env)
			for variable, want := range tc.want {
				if got[variable] != want {
					t.Errorf("%s = %q, want %q", variable, got[variable], want)
				}
				if n := named(env, variable); n != 1 {
					t.Errorf("%s is named %d times in %q", variable, n, env)
				}
			}
		})
	}
}

// On the pinned Oh My Pi, a repository whose .omp/config.yml turns remote compaction on reads it
// off under Apply's environment, and an operator overlay named after the baseline reads the
// operator's own endpoint: the baseline outranks the repository, and the operator outranks it.
// Without Apply the repository's endpoint is read, so a binary that stopped reading the
// repository's settings cannot pass the first row by accident.
func TestTheBaselineHoldsARepositoryOffAndTheOperatorOverridesIt(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	testbin.OMPHome(t, omp, home)
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".omp", "config.yml"), []byte("compaction:\n  remoteEndpoint: https://repository.example/compact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	if err := os.WriteFile(operator, []byte("compaction:\n  remoteEndpoint: https://operator.example/compact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		pod     []string
		noApply bool
		want    string
	}{
		"without the baseline, the repository's setting": {noApply: true, want: "https://repository.example/compact"},
		"the repository's setting held off":              {want: ""},
		"the operator's overlay wins":                    {pod: []string{"PI_CONFIG_FILES=" + operator}, want: "https://operator.example/compact"},
	} {
		t.Run(name, func(t *testing.T) {
			env := append([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, tc.pod...)
			if !tc.noApply {
				var err error
				if env, err = Apply(env, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(omp, "config", "get", "compaction.remoteEndpoint", "--json")
			cmd.Dir, cmd.Env = repo, env
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("omp config get: %v", err)
			}
			var setting struct{ Value string }
			if err := json.Unmarshal(out, &setting); err != nil {
				t.Fatalf("omp config get printed %q: %v", out, err)
			}
			if setting.Value != tc.want {
				t.Errorf("compaction.remoteEndpoint reads %q, want %q", setting.Value, tc.want)
			}
		})
	}
}
