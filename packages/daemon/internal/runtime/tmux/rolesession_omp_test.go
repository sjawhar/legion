package tmux

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/testbin"
)

// Every Legion role's Oh My Pi starts with async.enabled and bash.autoBackground.enabled on, in a
// pod (podsafety.Apply over the PI_CONFIG_FILES the Sandbox runtime composed, sandbox.
// roleOperatorEnv) and in a pane (PaneVariables), for a worker, the architect and the controller:
// Legion sets neither key and names no settings overlay of its own, so the pinned binary's own
// defaults for the protocol every role runs reach the agent (INFRA-338). Each read is asserted
// against the same binary's read under HOME and PATH alone, so what carries the proof is the
// relation between two reads of the pin rather than a default written out here (docs/solutions/
// testing/a-settings-proof-asserts-the-relation-between-two-reads-of-the-same-binary-never-a-
// literal-upstream-default-LEGION-578.md). PI_CONFIG_FILES is the operator's overlay and the
// role's own exactly: the overlay Legion used to write read both keys off under this pin, so an
// overlay that came back would fail both the value and the list.
func TestEveryRolesOhMyPiHasAsyncAndAutoBackgroundOnInAPodAndInAPane(t *testing.T) {
	omp := testbin.OMP(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	testbin.OMPHome(t, omp, home)
	// The working directory a role's Oh My Pi starts in, with no settings of its own, so the two
	// keys come from the binary and the overlays alone.
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	// An operator's own overlay (runtime.tmux.pi_config_files, or runtime.kubernetes.pod.env's
	// PI_CONFIG_FILES) and one role's own (role_config_files), neither naming either key, as an
	// operator's own do not.
	operator, roleOverlay := filepath.Join(dir, "operator.yml"), filepath.Join(dir, "role.yml")
	for file, body := range map[string]string{
		operator:    "compaction:\n  remoteEndpoint: https://operator.example/compact\n",
		roleOverlay: "retry:\n  modelFallback: true\n",
	} {
		if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bare := []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	keys := []string{"async.enabled", "bash.autoBackground.enabled"}
	// read is what omp answers for key under env, in the working directory a role starts in.
	read := func(t *testing.T, key string, env []string) bool {
		t.Helper()
		cmd := exec.Command(omp, "config", "get", key, "--json")
		cmd.Dir, cmd.Env = work, env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("omp config get %s: %v", key, err)
		}
		var setting struct{ Value any }
		if err := json.Unmarshal(out, &setting); err != nil {
			t.Fatalf("omp config get %s printed %q: %v", key, out, err)
		}
		on, ok := setting.Value.(bool)
		if !ok {
			t.Fatalf("omp config get %s read %#v, want a boolean", key, setting.Value)
		}
		return on
	}
	// What the pin answers with no Legion environment at all: the other read of each pair.
	noOverlay := map[string]bool{}
	for _, key := range keys {
		noOverlay[key] = read(t, key, bare)
	}
	// named is what env sets name to, the last entry winning as the child's own lookup does.
	named := func(env []string, name string) string {
		value := ""
		for _, pair := range env {
			if after, found := strings.CutPrefix(pair, name+"="); found {
				value = after
			}
		}
		return value
	}
	wantOverlays := operator + ":" + roleOverlay
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleArchitect, claim.RoleController} {
		spec := testSpec()
		if role == claim.RoleController {
			spec = controllerSpec(t, "omp")
		} else {
			spec.Role = role
		}
		for layout, environment := range map[string]func(*testing.T) []string{
			"a pod": func(t *testing.T) []string {
				t.Helper()
				env, err := podsafety.Apply(append(append([]string{}, bare...), "PI_CONFIG_FILES="+wantOverlays), t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				return env
			},
			"a pane": func(t *testing.T) []string {
				t.Helper()
				in := PaneInputs{StateDir: t.TempDir(), PiConfigFiles: []string{operator}, RoleConfigFiles: []string{roleOverlay}}
				variables, err := PaneVariables(spec, in, nil)
				if err != nil {
					t.Fatal(err)
				}
				return append(variables, bare...)
			},
		} {
			t.Run(string(role)+" in "+layout, func(t *testing.T) {
				env := environment(t)
				if got := named(env, "PI_CONFIG_FILES"); got != wantOverlays {
					t.Errorf("the %s's PI_CONFIG_FILES in %s = %q, want %q: Legion names no settings overlay of its own", role, layout, got, wantOverlays)
				}
				for _, key := range keys {
					got := read(t, key, env)
					if !got || got != noOverlay[key] {
						t.Errorf("the %s's Oh My Pi in %s reads %s %v, and the same binary under HOME and PATH alone reads it %v: want it on, as the pin answers with no overlay of Legion's",
							role, layout, key, got, noOverlay[key])
					}
				}
			})
		}
	}
}
