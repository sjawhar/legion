package podsafety

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// The overlay is the two turn-scoping keys, both off, and nothing else. A bash call Oh My Pi leaves
// running past its own threshold, under `--mode rpc` (every Legion role), moves to a background job
// a takeover's abort no longer reaches once it has moved — only the turn asking for it to finish
// does; with async on, the thermonuclear review pair a reviewer dispatches without `blocking: true`
// runs as background jobs that abort does not own, and its async result starts a new reviewer turn
// beside whatever claims the issue next — a CI-red takeover mid-review among them, exactly the turn
// boundary Quiesce exists to hold. supervise.Machine.Quiesce's own promise, that the outgoing worker
// "never writes the shared workspace beside the role the start hands it to", depends on both
// staying inside the turn the abort ends (LEGION-462 found the bash case live, legion-smoke
// LEGSMOKE-463, at commit 80d0c82b). Any other key would hold a repository's settings off, which
// nothing of Legion's does: the operator's overlay does that, where the operator wants it.
func TestTheOverlayHoldsExactlyTheTwoTurnScopingKeysOff(t *testing.T) {
	var pins map[string]any
	if err := yaml.Unmarshal(TurnScopeOverlay, &pins); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	leaves("", pins, got)
	want := map[string]any{"bash.autoBackground.enabled": false, "async.enabled": false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the overlay holds %v, want exactly %v", got, want)
	}
}

// leaves flattens a parsed YAML mapping into dotted paths, each to the scalar it holds.
func leaves(prefix string, node any, into map[string]any) {
	section, ok := node.(map[string]any)
	if !ok {
		into[prefix] = node
		return
	}
	for key, value := range section {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		leaves(path, value, into)
	}
}

// Variables are PI_CONFIG_FILES, which Apply composes, and the two that place a pod's sessions,
// both reserved to the runtime: nothing else is set on a pod's agent, so nothing of a repository's
// .env is held off beyond where its sessions go.
func TestVariablesAreTheOverlaysAndTheTwoThatPlaceSessions(t *testing.T) {
	want := []Variable{{Name: "PI_CONFIG_FILES"}, {Name: "PI_CONFIG_DIR", Reserved: placesSessions}, {Name: "OMP_SESSION_STORAGE", Reserved: placesSessions}}
	if got := Variables(); !slices.Equal(got, want) {
		t.Errorf("Variables() = %v, want %v", got, want)
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
			written := filepath.Join(state, TurnScopeFile)
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
			if string(body) != string(TurnScopeOverlay) || info.Mode().Perm() != 0o444 {
				t.Errorf("%s holds %d bytes at mode %v, want the turn-scope overlay at 0444", written, len(body), info.Mode().Perm())
			}
		})
	}
}

// Each baseline variable is set only where the pod's environment leaves it unset or empty — the
// case in which Oh My Pi would fill it from a repository's .env — so a value the pod carries wins,
// the Sandbox runtime's session database among them, which it names before Apply runs (Apply does
// not know who set it; the Sandbox runtime is what refuses an operator's pod setting either,
// sandbox.CheckPod). Nothing beyond the two and PI_CONFIG_FILES is set.
func TestApplySetsEachBaselineVariableOnlyWhereThePodLeavesItUnset(t *testing.T) {
	baseline := map[string]string{"PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "file"}
	for name, tc := range map[string]struct {
		pod, want map[string]string
	}{
		"unset":                {map[string]string{}, baseline},
		"the pod's own value":  {map[string]string{"OMP_SESSION_STORAGE": "sql"}, map[string]string{"PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "sql"}},
		"empty, as .env fills": {map[string]string{"OMP_SESSION_STORAGE": ""}, baseline},
		"another config root":  {map[string]string{"PI_CONFIG_DIR": ".config/omp"}, map[string]string{"PI_CONFIG_DIR": ".config/omp", "OMP_SESSION_STORAGE": "file"}},
		"the runtime's session database": {
			map[string]string{"OMP_SESSION_STORAGE": "sql", "OMP_SESSION_SQL_DSN_FILE": "/var/run/legion/providers/OMP_SESSION_SQL_DSN"},
			map[string]string{"PI_CONFIG_DIR": ".omp", "OMP_SESSION_STORAGE": "sql", "OMP_SESSION_SQL_DSN_FILE": "/var/run/legion/providers/OMP_SESSION_SQL_DSN"},
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
			for variable := range got {
				if _, pods := tc.pod[variable]; !pods && variable != "PI_CONFIG_FILES" && tc.want[variable] == "" {
					t.Errorf("Apply set %s=%q, which is neither the pod's nor the baseline's", variable, got[variable])
				}
			}
		})
	}
}

// On the pinned Oh My Pi, a repository's own settings reach the agent under Apply's environment as
// they reach any agent session: its .omp/config.yml's remote compaction endpoint is read as the
// repository set it, and an operator overlay named after Legion's reads the operator's own endpoint
// over it. Only the two turn-scoping keys are held off, whatever the repository sets them to.
// Without Apply the repository turns those on, so an Oh My Pi that stopped reading the repository's
// settings cannot pass the held-off rows by accident.
func TestARepositorysSettingsReachTheAgentUnderTheOperatorsAndTheTurnScopeHolds(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	testbin.OMPHome(t, omp, home)
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	repository := "compaction:\n  remoteEndpoint: https://repository.example/compact\nbash:\n  autoBackground:\n    enabled: true\nasync:\n  enabled: true\n"
	if err := os.WriteFile(filepath.Join(repo, ".omp", "config.yml"), []byte(repository), 0o600); err != nil {
		t.Fatal(err)
	}
	operator := filepath.Join(dir, "operator.yml")
	if err := os.WriteFile(operator, []byte("compaction:\n  remoteEndpoint: https://operator.example/compact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		setting string
		pod     []string
		noApply bool
		want    any
	}{
		"the repository's endpoint, without the baseline":      {setting: "compaction.remoteEndpoint", noApply: true, want: "https://repository.example/compact"},
		"the repository's endpoint reaches the agent":          {setting: "compaction.remoteEndpoint", want: "https://repository.example/compact"},
		"the operator's overlay outranks the repository's":     {setting: "compaction.remoteEndpoint", pod: []string{"PI_CONFIG_FILES=" + operator}, want: "https://operator.example/compact"},
		"the repository turns autoBackground on, without":      {setting: "bash.autoBackground.enabled", noApply: true, want: true},
		"autoBackground held off whatever the repository sets": {setting: "bash.autoBackground.enabled", want: false},
		"async held off whatever the repository sets":          {setting: "async.enabled", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			env := append([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, tc.pod...)
			if !tc.noApply {
				var err error
				if env, err = Apply(env, t.TempDir()); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(omp, "config", "get", tc.setting, "--json")
			cmd.Dir, cmd.Env = repo, env
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("omp config get: %v", err)
			}
			var setting struct{ Value any }
			if err := json.Unmarshal(out, &setting); err != nil {
				t.Fatalf("omp config get printed %q: %v", out, err)
			}
			if setting.Value != tc.want {
				t.Errorf("%s reads %#v, want %#v", tc.setting, setting.Value, tc.want)
			}
		})
	}
}
