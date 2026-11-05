package tmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// memoryRuntime is a fake server runtime over the operator's overlay, the tester's own, and the
// project's memory overlays, all on stateDir, so two launches differ by the spec alone. A nil
// memory list is a daemon whose project names no `memory` block at all.
func memoryRuntime(t *testing.T, stateDir string, memory []string) *fakeServerRuntime {
	t.Helper()
	return newFakeServerRuntimeWith(t, staticCredential, nil, func(o *Options) {
		o.StateDir = stateDir
		o.PiConfigFiles = []string{"/etc/legion-operator/overlay.yml"}
		o.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {"/etc/legion-operator/tester.yml"}}
		o.MemoryConfigFiles = memory
	})
}

// memoryOverlays are the project's `memory.config_files` in every test here.
var memoryOverlays = []string{"/etc/legion-memory/memory.yml", "/etc/legion-memory/recall.yml"}

// newWindow is the new-window argv of one launch of spec by f, and the launch's -e pairs.
func newWindow(t *testing.T, f *fakeServerRuntime, spec runtime.SpawnSpec) ([]string, []string) {
	t.Helper()
	if _, err := f.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	for _, argv := range f.argvs {
		if verb(argv) == "new-window" {
			return argv, f.pairs(t)
		}
	}
	t.Fatal("no new-window reached the fake server")
	return nil, nil
}

// A pane whose tree was drawn into the memory group names the project's memory overlays last in
// PI_CONFIG_FILES: the operator's own first, then the role's, then the memory block's, in the
// order the operator wrote them. Its system prompt ends with the memory instructions, after the
// deployment's own.
func TestAMemoryGroupPaneNamesTheMemoryOverlaysLast(t *testing.T) {
	stateDir := t.TempDir()
	instructions := filepath.Join(t.TempDir(), "memory-instructions.md")
	if err := os.WriteFile(instructions, []byte("# Memory instructions (omp)\n\nRead the tree's memory.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := memoryRuntime(t, stateDir, memoryOverlays)
	spec := f.spec(t, claim.RoleTester)
	spec.Memory = true
	spec.Prompt.MemoryInstructionsPath = instructions

	argv, pairs := newWindow(t, f, spec)
	want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/tester.yml:" +
		"/etc/legion-memory/memory.yml:/etc/legion-memory/recall.yml"
	if !slices.Contains(pairs, want) {
		t.Errorf("the memory pane's pairs = %q, want %q", pairs, want)
	}
	command := argv[len(argv)-1]
	if !strings.Contains(command, "$(cat "+instructions+")") {
		t.Errorf("the memory pane's command =\n%s\nwant it to `cat` %s", command, instructions)
	}
}

// A launch for an issue whose tree carries no memory group builds the pane command a deployment
// with no memory block at all builds, byte for byte: the block reaches only the panes of the trees
// the draw gave it, so moving a project onto memory changes no other pane. Memory before the
// block and after it are the same pane.
func TestNoMemoryBeforeOrAfterTheBlockBuildsTheSamePane(t *testing.T) {
	stateDir := t.TempDir()
	// One spec for both launches, so the two differ by the runtime's memory overlays alone.
	spec := memoryRuntime(t, stateDir, nil).spec(t, claim.RoleTester)

	without, withoutPairs := newWindow(t, memoryRuntime(t, stateDir, nil), spec)
	with, withPairs := newWindow(t, memoryRuntime(t, stateDir, memoryOverlays), spec)
	if !slices.Equal(with, without) {
		t.Errorf("a no-memory tree's pane under the block =\n%q\nwant the one a daemon with no block builds:\n%q", with, without)
	}
	want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/tester.yml"
	for _, pairs := range [][]string{withoutPairs, withPairs} {
		if !slices.Contains(pairs, want) {
			t.Errorf("a no-memory tree's pairs = %q, want %q", pairs, want)
		}
	}
}

// The controller's pane never names the memory overlays, whatever the project's block says: a
// memory group is a tree's, and the controller runs in none. Its spec carries no memory, and the
// shared validator refuses one that does.
func TestTheControllersPaneNamesNoMemoryOverlay(t *testing.T) {
	f := memoryRuntime(t, t.TempDir(), memoryOverlays)
	_, pairs := newWindow(t, f, controllerSpec(t, "omp"))
	want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml"
	if !slices.Contains(pairs, want) {
		t.Errorf("the controller's pairs = %q, want %q", pairs, want)
	}

	refused := controllerSpec(t, "omp")
	refused.Memory = true
	wantErr := "spawn legion-omp-controller: the controller runs in no tree, so it is in no memory group"
	if _, err := memoryRuntime(t, t.TempDir(), memoryOverlays).Spawn(context.Background(), refused); err == nil || err.Error() != wantErr {
		t.Fatalf("Spawn a controller in the memory group = %v, want %q", err, wantErr)
	}
}

// New refuses a memory overlay PI_CONFIG_FILES cannot name, as it refuses the operator's and a
// role's.
func TestNewRefusesAMemoryOverlayPIConfigFilesCannotName(t *testing.T) {
	for _, file := range []string{"memory.yml", "/etc/a:b.yml"} {
		opts := paneEnvOptions(t, nil, nil)
		opts.MemoryConfigFiles = []string{file}
		want := `tmux runtime: Oh My Pi overlay "` + file + `" is not an absolute path PI_CONFIG_FILES can name`
		if _, err := New(opts); err == nil || err.Error() != want {
			t.Errorf("New with the memory overlay %q = %v, want %q", file, err, want)
		}
	}
}

// A launch whose memory instructions file is not there is refused before anything touches tmux, as
// a missing role prompt or deployment instructions file is.
func TestAMissingMemoryInstructionsFileRefusesTheLaunch(t *testing.T) {
	f := memoryRuntime(t, t.TempDir(), memoryOverlays)
	spec := f.spec(t, claim.RoleTester)
	spec.Memory = true
	spec.Prompt.MemoryInstructionsPath = filepath.Join(t.TempDir(), "absent.md")
	if _, err := f.Spawn(context.Background(), spec); err == nil {
		t.Fatal("a launch over a missing memory instructions file was not refused")
	}
	if len(f.argvs) != 0 {
		t.Errorf("a refused launch ran tmux: %q", f.argvs)
	}
}

// PaneVariables tells a caller exactly what a memory-group pane gets, the memory overlays among
// its PI_CONFIG_FILES, so the rigs under packages/pi-legion/scripts run Oh My Pi under the pane's
// own environment.
func TestPaneVariablesCarryTheMemoryOverlays(t *testing.T) {
	in := PaneInputs{
		StateDir: t.TempDir(), PiConfigFiles: []string{"/etc/legion-operator/overlay.yml"},
		RoleConfigFiles: []string{"/etc/legion-operator/tester.yml"}, MemoryConfigFiles: memoryOverlays,
	}
	variables, err := PaneVariables(testSpec(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "PI_CONFIG_FILES=/etc/legion-operator/overlay.yml:/etc/legion-operator/tester.yml:" +
		"/etc/legion-memory/memory.yml:/etc/legion-memory/recall.yml"
	if !slices.Contains(variables, want) {
		t.Errorf("PaneVariables = %q, want %q", variables, want)
	}
}
