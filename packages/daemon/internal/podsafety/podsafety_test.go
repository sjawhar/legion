package podsafety

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// Variables are the two that place a pod's sessions, both reserved to the runtime, and nothing
// else: nothing else is set on a pod's agent, so nothing of a repository's .env is held off beyond
// where its sessions go, and no settings overlay of Legion's reaches PI_CONFIG_FILES.
func TestVariablesAreTheTwoThatPlaceSessions(t *testing.T) {
	want := []Variable{{Name: "PI_CONFIG_DIR", Reserved: placesSessions}, {Name: "OMP_SESSION_STORAGE", Reserved: placesSessions}}
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

// Apply leaves PI_CONFIG_FILES as the pod carries it and writes nothing under the state
// directory: Legion names no settings overlay of its own, so the overlays a role's Oh My Pi reads
// are the operator's own and the role's, which the runtime composed before the shim ran, and a
// repository's settings sit under those alone.
func TestApplyLeavesTheOverlaysThePodCarriesAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		operator []string
		want     string
		set      bool
	}{
		"the operator's overlays":  {operator: []string{"PI_CONFIG_FILES=/etc/op.yml:/etc/op2.yml"}, want: "/etc/op.yml:/etc/op2.yml", set: true},
		"an empty PI_CONFIG_FILES": {operator: []string{"PI_CONFIG_FILES="}, want: "", set: true},
		"no overlay of the pod's":  {},
	} {
		t.Run(name, func(t *testing.T) {
			state := t.TempDir()
			env, err := Apply(append([]string{"HOME=/home/legion"}, tc.operator...), state)
			if err != nil {
				t.Fatal(err)
			}
			got, set := envMap(env)["PI_CONFIG_FILES"]
			if got != tc.want || set != tc.set {
				t.Errorf("PI_CONFIG_FILES = %q (set %v), want %q (set %v)", got, set, tc.want, tc.set)
			}
			if n := named(env, "PI_CONFIG_FILES"); n > 1 {
				t.Errorf("PI_CONFIG_FILES is named %d times in %q", n, env)
			}
			entries, err := os.ReadDir(state)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("Apply wrote %v under the state directory, want nothing: Legion writes no settings overlay", entries)
			}
		})
	}
}

// Each baseline variable is set only where the pod's environment leaves it unset or empty — the
// case in which Oh My Pi would fill it from a repository's .env — so a value the pod carries wins,
// the Sandbox runtime's session database among them, which it names before Apply runs (Apply does
// not know who set it; the Sandbox runtime is what refuses an operator's pod setting either,
// sandbox.CheckPod). Nothing beyond the two is set, PI_CONFIG_FILES included.
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
				if _, pods := tc.pod[variable]; !pods && tc.want[variable] == "" {
					t.Errorf("Apply set %s=%q, which is neither the pod's nor the baseline's", variable, got[variable])
				}
			}
		})
	}
}

// EnsureStateHome makes the directory Oh My Pi resolves its state root to under XDG_STATE_HOME —
// `omp/profiles/<profile>` for the profile OMP_PROFILE (else PI_PROFILE) names, `omp` for the
// default profile — since Oh My Pi reads the variable only where that directory already exists;
// with no state home it makes nothing, it refuses a path it cannot make, naming it, and it refuses
// an environment under which Oh My Pi would root its state elsewhere than the directory it made,
// naming both.
func TestEnsureStateHomeMakesOhMyPisStateRootUnderTheStateHome(t *testing.T) {
	for name, tc := range map[string]struct {
		environ func(stateHome string) []string
		want    string
	}{
		"the image's profile": {
			func(home string) []string { return []string{"XDG_STATE_HOME=" + home, "OMP_PROFILE=legion"} },
			"omp/profiles/legion",
		},
		"PI_PROFILE where OMP_PROFILE is undefined": {
			func(home string) []string { return []string{"XDG_STATE_HOME=" + home, "PI_PROFILE=work"} },
			"omp/profiles/work",
		},
		"no profile": {
			func(home string) []string { return []string{"XDG_STATE_HOME=" + home, "HOME=/home/legion"} },
			"omp",
		},
		"an empty OMP_PROFILE, the default profile over PI_PROFILE": {
			func(home string) []string {
				return []string{"XDG_STATE_HOME=" + home, "OMP_PROFILE=", "PI_PROFILE=work"}
			},
			"omp",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateHome := filepath.Join(t.TempDir(), "state", "tester")
			if err := EnsureStateHome(tc.environ(stateHome)); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(stateHome, tc.want))
			if err != nil {
				t.Fatalf("%s under the state home: %v", tc.want, err)
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("%s is %v, want a directory at mode 0700", tc.want, info.Mode())
			}
		})
	}
	t.Run("no state home", func(t *testing.T) {
		dir := t.TempDir()
		for _, environ := range [][]string{{"OMP_PROFILE=legion", "HOME=" + dir}, {"XDG_STATE_HOME=", "OMP_PROFILE=legion", "HOME=" + dir}} {
			if err := EnsureStateHome(environ); err != nil {
				t.Fatalf("%q: %v", environ, err)
			}
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("with no state home, EnsureStateHome made %v", entries)
		}
	})
	t.Run("a file where the directory should go", func(t *testing.T) {
		stateHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(stateHome, "omp"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := EnsureStateHome([]string{"XDG_STATE_HOME=" + stateHome, "OMP_PROFILE=legion"})
		if err == nil || !strings.Contains(err.Error(), filepath.Join(stateHome, "omp", "profiles", "legion")) {
			t.Fatalf("EnsureStateHome = %v, want an error naming %s", err, filepath.Join(stateHome, "omp", "profiles", "legion"))
		}
	})
	t.Run("a profile Oh My Pi refuses", func(t *testing.T) {
		stateHome := t.TempDir()
		err := EnsureStateHome([]string{"XDG_STATE_HOME=" + stateHome, "OMP_PROFILE=../escape"})
		if err == nil || !strings.Contains(err.Error(), `"../escape"`) {
			t.Fatalf("EnsureStateHome = %v, want a refusal naming the profile", err)
		}
		if entries, _ := os.ReadDir(stateHome); len(entries) != 0 {
			t.Errorf("a refused profile made %v", entries)
		}
	})
	t.Run("an agent directory elsewhere turns the state home off", func(t *testing.T) {
		home, stateHome := t.TempDir(), t.TempDir()
		err := EnsureStateHome([]string{"XDG_STATE_HOME=" + stateHome, "HOME=" + home, "PI_CODING_AGENT_DIR=" + filepath.Join(home, "elsewhere")})
		made, read := filepath.Join(stateHome, "omp"), filepath.Join(home, ".omp")
		if err == nil || !strings.Contains(err.Error(), made) || !strings.Contains(err.Error(), read) {
			t.Fatalf("EnsureStateHome = %v, want a refusal naming %s and %s", err, made, read)
		}
	})
}

// EnsureScratch makes every directory the runtime tells a role to write what it writes without
// being told where — TMPDIR, XDG_CACHE_HOME, XDG_DATA_HOME, GOCACHE, GOMODCACHE and
// BUN_INSTALL_CACHE_DIR — with every parent, since `go` and `bun` make their own cache
// directories but nothing makes the temporary directory a role is told, and mktemp fails where it
// does not exist. A name the environment leaves unset or empty, or sets to a relative path, makes
// nothing; XDG_STATE_HOME is EnsureStateHome's alone, untouched here; and a directory that cannot
// be made is a refusal naming the variable and the path, so a role whose volume cannot be written
// never starts half-configured.
func TestEnsureScratchMakesEveryDirectoryTheRoleIsToldToWriteUnder(t *testing.T) {
	t.Run("every told directory, with its parents", func(t *testing.T) {
		scratch := filepath.Join(t.TempDir(), "legion", "scratch", "tester")
		told := map[string]string{
			"TMPDIR":                filepath.Join(scratch, "tmp"),
			"XDG_CACHE_HOME":        filepath.Join(scratch, "cache"),
			"XDG_DATA_HOME":         filepath.Join(scratch, "data"),
			"GOCACHE":               filepath.Join(scratch, "go", "build"),
			"GOMODCACHE":            filepath.Join(scratch, "go", "mod"),
			"BUN_INSTALL_CACHE_DIR": filepath.Join(scratch, "bun", "install", "cache"),
		}
		stateHome := filepath.Join(scratch, "state")
		environ := []string{"XDG_STATE_HOME=" + stateHome, "XDG_CONFIG_HOME=/run/legion/config", "OMP_PROFILE=legion"}
		for name, dir := range told {
			environ = append(environ, name+"="+dir)
		}
		if err := EnsureScratch(environ); err != nil {
			t.Fatal(err)
		}
		for name, dir := range told {
			info, err := os.Stat(dir)
			if err != nil {
				t.Errorf("the directory %s names: %v", name, err)
				continue
			}
			if !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Errorf("%s names %s, which is %v, want a directory at mode 0700", name, dir, info.Mode())
			}
		}
		if _, err := os.Stat(stateHome); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the state home %s: %v, want it left to EnsureStateHome", stateHome, err)
		}
	})
	for name, environ := range map[string][]string{
		"unset": {"XDG_CONFIG_HOME=/run/legion/config", "OMP_PROFILE=legion"},
		"empty": {"TMPDIR=", "XDG_CACHE_HOME=", "XDG_DATA_HOME=", "GOCACHE=", "GOMODCACHE=", "BUN_INSTALL_CACHE_DIR="},
		"relative": {
			"TMPDIR=tmp", "XDG_CACHE_HOME=.cache", "XDG_DATA_HOME=share/data",
			"GOCACHE=go/build", "GOMODCACHE=go/mod", "BUN_INSTALL_CACHE_DIR=bun/install/cache",
		},
	} {
		t.Run("a value that is "+name, func(t *testing.T) {
			dir := t.TempDir()
			// The working directory a relative value would be made under, were it the shim's to
			// resolve: nothing of it is the shim's, so nothing lands here.
			t.Chdir(dir)
			if err := EnsureScratch(environ); err != nil {
				t.Fatalf("%q: %v", environ, err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("with every value %s, EnsureScratch made %v", name, entries)
			}
		})
	}
	t.Run("a file where the directory should go", func(t *testing.T) {
		scratch := t.TempDir()
		if err := os.WriteFile(filepath.Join(scratch, "go"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		blocked := filepath.Join(scratch, "go", "build")
		err := EnsureScratch([]string{"TMPDIR=" + filepath.Join(scratch, "tmp"), "GOCACHE=" + blocked})
		if err == nil || !strings.Contains(err.Error(), "GOCACHE") || !strings.Contains(err.Error(), blocked) {
			t.Fatalf("EnsureScratch = %v, want a refusal naming GOCACHE and %s", err, blocked)
		}
	})
}

// On the pinned Oh My Pi, a repository's own settings reach the agent under Apply's environment as
// they reach any agent session: its .omp/config.yml's remote compaction endpoint is read as the
// repository set it, and an operator overlay named in the pod's PI_CONFIG_FILES reads the
// operator's own endpoint over it. The baseline holds nothing of a repository's settings off, so
// the endpoint the repository sets must read the same with Apply and without it.
func TestARepositorysSettingsReachTheAgentUnderTheOperators(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	testbin.OMPHome(t, omp, home)
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".omp"), 0o700); err != nil {
		t.Fatal(err)
	}
	repository := "compaction:\n  remoteEndpoint: https://repository.example/compact\n"
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
		"the repository's endpoint, without the baseline":  {setting: "compaction.remoteEndpoint", noApply: true, want: "https://repository.example/compact"},
		"the repository's endpoint reaches the agent":      {setting: "compaction.remoteEndpoint", want: "https://repository.example/compact"},
		"the operator's overlay outranks the repository's": {setting: "compaction.remoteEndpoint", pod: []string{"PI_CONFIG_FILES=" + operator}, want: "https://operator.example/compact"},
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

// The pinned Oh My Pi roots its state under the state home only where the shim made the profile's
// directory under it: with EnsureStateHome applied, `omp models list` (which writes a log; `omp
// config get` writes none) logs under `$XDG_STATE_HOME/omp/profiles/legion/logs` and the config
// root's profile gets no `logs`; without it, the log lands under `$HOME/.omp/profiles/legion/logs`
// and the state home stays empty. Either way ompdirs.StateRoot answers the root the log landed
// under, so the port, the shim and the binary agree on the rule.
func TestThePinnedOhMyPiRootsItsStateUnderTheStateHomeOnlyWhereTheShimMadeTheProfileDirectory(t *testing.T) {
	omp := testbin.OMP(t)
	for name, ensured := range map[string]bool{
		"the shim made the profile directory": true,
		"without the shim":                    false,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(dir, "home")
			testbin.OMPHome(t, omp, home)
			stateHome := filepath.Join(dir, "state", "tester")
			if err := os.MkdirAll(stateHome, 0o700); err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(dir, "work")
			if err := os.Mkdir(work, 0o700); err != nil {
				t.Fatal(err)
			}
			env := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "OMP_PROFILE=legion", "XDG_STATE_HOME=" + stateHome}
			if ensured {
				if err := EnsureStateHome(env); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(omp, "models", "list")
			cmd.Dir, cmd.Env = work, env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("omp models list: %v\n%s", err, out)
			}
			underStateHome := filepath.Join(stateHome, "omp", "profiles", "legion")
			underHome := filepath.Join(home, ".omp", "profiles", "legion")
			want, other := underStateHome, underHome
			if !ensured {
				want, other = underHome, underStateHome
			}
			if logs, err := filepath.Glob(filepath.Join(want, "logs", "omp.*.log")); err != nil || len(logs) == 0 {
				t.Errorf("logs under %s: %v, %v, want Oh My Pi's log there", want, logs, err)
			}
			if _, err := os.Stat(filepath.Join(other, "logs")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("stat %s/logs: %v, want none", other, err)
			}
			if !ensured {
				if entries, err := os.ReadDir(stateHome); err != nil || len(entries) != 0 {
					t.Errorf("the state home holds %v, %v, want nothing without the shim", entries, err)
				}
			}
			if root, _, err := ompdirs.StateRoot(lookup(env), work); err != nil || root != want {
				t.Errorf("ompdirs.StateRoot = %s, %v, want %s, where the log landed", root, err, want)
			}
		})
	}
}
