package tmux

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// The operator's `runtime.tmux` block reaches every pane: its plain variables after the runtime's
// own and before the spec's, sorted, and its Oh My Pi overlays in PI_CONFIG_FILES ahead of the
// turn-scoping overlay, which stays last so its two keys outrank theirs.
func TestThePaneIsToldTheOperatorsVariablesAndOverlays(t *testing.T) {
	spec := testSpec()
	in := PaneInputs{
		StateDir: "/state", Workspace: "/state/workspaces/LEGION-43", DaemonURL: "http://127.0.0.1:13370",
		OperatorEnv:   map[string]string{"LAKE_CACHE_DIR": "/legion/lake-cache", "DISPATCH_PUBLIC_URL": "https://dispatch.example"},
		PiConfigFiles: []string{"/etc/legion-operator/overlay.yml", "/etc/legion-operator/project.yml"},
	}
	pairs := panePairs(spec, in, secretFiles("/state", spec))
	var values []string
	for i := 1; i < len(pairs); i += 2 {
		values = append(values, pairs[i])
	}
	want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/project.yml:/state/" + podsafety.TurnScopeFile
	if !slices.Contains(values, want) {
		t.Errorf("a pane's pairs = %q, want %q among them", values, want)
	}
	order := []string{"XDG_STATE_HOME=/state/home/.local/state", "DISPATCH_PUBLIC_URL=https://dispatch.example", "LAKE_CACHE_DIR=/legion/lake-cache", "JJ_USER=legion-tester"}
	last := -1
	for _, pair := range order {
		at := slices.Index(values, pair)
		if at < 0 || at < last {
			t.Fatalf("a pane's pairs = %q, want %q in that order", values, order)
		}
		last = at
	}
}

// New refuses, before any pane opens, an operator pane variable that would change what a pane is
// told by anything but the operator — a name the runtime sets on a tree or controller pane, PATH,
// the git identity each launch sets, a provider key or its pointer, a credential-shaped name — and
// an overlay PI_CONFIG_FILES cannot name.
func TestNewRefusesAnOperatorPaneVariableTheRuntimeOwns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		overlays []string
		want     string
	}{
		{"a tree pane's variable", map[string]string{"LEGION_ISSUE": "X-1"}, nil, "tmux runtime: pane variable LEGION_ISSUE is one the runtime sets on every pane itself"},
		{"the controller's marker", map[string]string{"LEGION_CONTROLLER": "1"}, nil, "tmux runtime: pane variable LEGION_CONTROLLER is one the runtime sets on every pane itself"},
		{"PATH", map[string]string{"PATH": "/usr/bin"}, nil, "tmux runtime: pane variable PATH is one the runtime sets on every pane itself"},
		{"the git identity", map[string]string{"GIT_AUTHOR_NAME": "someone"}, nil, "tmux runtime: pane variable GIT_AUTHOR_NAME is part of the git identity each launch sets for its role"},
		{"a credential", map[string]string{"LEDGER_TOKEN": "hunter2"}, nil, "tmux runtime: pane variable LEDGER_TOKEN is credential-shaped: a secret reaches a pane as a file (provider_keys), never as a value"},
		{"not a variable name", map[string]string{"1X": "y"}, nil, `tmux runtime: pane variable "1X" is not an environment variable name`},
		{"a relative overlay", nil, []string{"overlay.yml"}, `tmux runtime: Oh My Pi overlay "overlay.yml" is not an absolute path PI_CONFIG_FILES can name`},
		{"an overlay holding the separator", nil, []string{"/etc/a:b.yml"}, `tmux runtime: Oh My Pi overlay "/etc/a:b.yml" is not an absolute path PI_CONFIG_FILES can name`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(paneEnvOptions(t, tc.env, tc.overlays))
			if err == nil || err.Error() != tc.want {
				t.Fatalf("New = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := New(paneEnvOptions(t, map[string]string{"ELAN_HOME": "/opt/elan", "LEDGER_DSN_FILE": "/etc/legion-ledger/dsn"}, []string{"/etc/legion-operator/overlay.yml"})); err != nil {
		t.Errorf("New with plain variables, a _FILE pointer and an absolute overlay: %v", err)
	}
}

// paneEnvOptions are the Options of a runtime given the operator's pane variables and overlays.
func paneEnvOptions(t *testing.T, env map[string]string, overlays []string) Options {
	t.Helper()
	requireTmux(t)
	return Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=/usr/bin:/bin"},
		Executable: func() (string, error) { return "/opt/legion", nil }, GitHubCredential: staticCredential, GH: fakeGH, RepositoryOwner: acmeOwner,
		PaneEnv: env, PiConfigFiles: overlays,
	}
}

// A spec variable, or a secret's pointer, of a name the operator's pane variables set would have
// one of the two silently win, so the launch is refused before tmux is asked anything.
func TestALaunchCollidingWithAnOperatorPaneVariableIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*runtime.SpawnSpec)
		want   string
	}{
		{"a spec variable", func(s *runtime.SpawnSpec) { s.Env["ELAN_HOME"] = "/home/me/.elan" },
			"spawn legion-omp-legion-43-tester: Env sets ELAN_HOME, which runtime.tmux.pane_env sets on every pane"},
		{"a secret's pointer", func(s *runtime.SpawnSpec) { s.Secrets["LEDGER_DSN"] = "dsn" },
			"spawn legion-omp-legion-43-tester: secret LEDGER_DSN's pointer LEDGER_DSN_FILE is set on every pane by runtime.tmux.pane_env"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeServerRuntime(t, staticCredential, nil)
			f.operatorEnv = map[string]string{"ELAN_HOME": "/opt/elan", "LEDGER_DSN_FILE": "/etc/legion-ledger/dsn"}
			spec := f.spec(t, claim.RoleTester)
			tc.mutate(&spec)
			if _, err := f.Spawn(context.Background(), spec); err == nil || err.Error() != tc.want {
				t.Fatalf("Spawn = %v, want %q", err, tc.want)
			}
			if len(f.argvs) != 0 {
				t.Errorf("a refused launch ran tmux: %q", f.argvs)
			}
		})
	}
}

// The boot gate probes a pane's Oh My Pi under the environment a pane gets: the operator's
// variables and overlays among it (gateEnvironment, internal/daemon), which PaneEnvironment and the
// block compose as panePairs does; a pane launched by a runtime given the block is told the same.
func TestALaunchedPaneIsToldTheOperatorsBlock(t *testing.T) {
	f := newFakeServerRuntime(t, staticCredential, nil)
	f.operatorEnv = map[string]string{"LAKE_CACHE_DIR": "/legion/lake-cache"}
	f.piConfigFiles = []string{"/etc/legion-operator/overlay.yml"}
	spec := f.spec(t, claim.RoleTester)
	if _, err := f.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	pairs := f.pairs(t)
	for _, want := range []string{"LAKE_CACHE_DIR=/legion/lake-cache", "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:" + filepath.Join(f.stateDir, podsafety.TurnScopeFile)} {
		if !slices.Contains(pairs, want) {
			t.Errorf("the pane's pairs = %q, want %q", pairs, want)
		}
	}
	for _, pair := range pairs {
		if strings.HasPrefix(pair, "PATH=") {
			t.Errorf("PATH rode a -e pair: %q", pair)
		}
	}
}
