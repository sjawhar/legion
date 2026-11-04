package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// memoryFiles are a project's `memory.config_files` in every test here.
var memoryFiles = []string{"/etc/legion-memory/memory.yml", "/etc/legion-memory/recall.yml"}

// memoryConfig is a tmux configuration whose one project runs its memory block at mode.
func memoryConfig(mode config.MemoryMode) config.Config {
	return config.Config{
		Project: "test",
		Runtime: config.Runtime{Name: "tmux"},
		Projects: map[string]config.Project{"test": {
			Memory: config.Memory{Mode: mode, ConfigFiles: memoryFiles, InstructionsPath: "/etc/legion-memory/instructions.md"},
		}},
	}
}

// A project's memory overlays reach the tmux runtime's Options as the file settled them, and a
// project at mode off hands over none: such a deployment builds the pane it built before the block
// existed, whatever its other memory keys say.
func TestMemoryConfigFilesReachTheTmuxRuntimeUnlessTheModeIsOff(t *testing.T) {
	for _, tc := range []struct {
		mode config.MemoryMode
		want []string
	}{
		{config.MemoryOn, memoryFiles},
		{config.MemoryRandom, memoryFiles},
		{config.MemoryOff, nil},
		{"", nil},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			got := tmuxOptions(memoryConfig(tc.mode), "test", nil, "", "", quietLogger()).MemoryConfigFiles
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tmux MemoryConfigFiles = %v, want %v", got, tc.want)
			}
		})
	}
}

// memorySpecs is a spec builder for a project at mode whose memory instructions boot materialized,
// over a reader that answers memory for the tree LEGION-1 alone; a nil reader is a daemon with no
// workflow, which records no tree.
func memorySpecs(t *testing.T, mode config.MemoryMode, instructions string, reader bool) specs {
	t.Helper()
	composer, err := prompts.New(t.TempDir())
	if err != nil {
		t.Fatalf("compose the shipped prompts: %v", err)
	}
	s := specs{
		stateDir: t.TempDir(), project: "test", prompts: composer, repositories: noRepositoriesView,
		instructions: "/state/deployment-instructions.md", memoryInstructions: instructions,
		memory: config.Memory{Mode: mode, ConfigFiles: memoryFiles, InstructionsPath: "/etc/legion-memory/instructions.md"},
	}
	if reader {
		s.memoryGroup = func(_ context.Context, tree string) (bool, error) { return tree == "LEGION-1", nil }
	}
	return s
}

// memorySpawn is the spec s builds for role on issue of tree.
func memorySpawn(t *testing.T, s specs, tree, issue string, role claim.Role) runtime.SpawnSpec {
	t.Helper()
	token, err := claim.NewToken("test", issue, role)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: token, Project: "test", Issue: issue, Tree: tree, Role: role})
	if err != nil {
		t.Fatalf("SpawnSpec %s: %v", token, err)
	}
	return spec
}

// Every role of a tree the draw put in the memory group launches with memory: the spec says so, so
// each runtime names the project's overlays last, and its prompt ends with the memory instructions
// the daemon materialized at boot, after the deployment's own. A role of a tree with no group, and
// the daemon's controller, carry neither.
func TestMemoryReachesOnlyTheRolesOfAMemoryTree(t *testing.T) {
	const instructions = "/state/memory-instructions.md"
	s := memorySpecs(t, config.MemoryRandom, instructions, true)
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
		spec := memorySpawn(t, s, "LEGION-1", "LEGION-2", role)
		if !spec.Memory || spec.Prompt.MemoryInstructionsPath != instructions {
			t.Errorf("the %s of a memory tree: Memory = %v, instructions = %q; want true and %q",
				role, spec.Memory, spec.Prompt.MemoryInstructionsPath, instructions)
		}
		if spec.Prompt.DeploymentInstructionsPath != s.instructions {
			t.Errorf("the %s of a memory tree: deployment instructions = %q, want %q", role, spec.Prompt.DeploymentInstructionsPath, s.instructions)
		}
	}
	noMemory := memorySpawn(t, s, "LEGION-7", "LEGION-8", claim.RoleTester)
	if noMemory.Memory || noMemory.Prompt.MemoryInstructionsPath != "" {
		t.Errorf("a tree with no group: Memory = %v, instructions = %q; want false and \"\"", noMemory.Memory, noMemory.Prompt.MemoryInstructionsPath)
	}

	controller, err := s.SpawnSpec(context.Background(), supervise.Claim{Token: claim.ControllerToken("test"), Project: "test", Role: claim.RoleController})
	if err != nil {
		t.Fatalf("the controller's SpawnSpec: %v", err)
	}
	if controller.Memory || controller.Prompt.MemoryInstructionsPath != "" {
		t.Errorf("the controller: Memory = %v, instructions = %q; want false and \"\"", controller.Memory, controller.Prompt.MemoryInstructionsPath)
	}
}

// With the mode off nothing reaches a launch, whatever a tree's record carries: the reader is not
// even asked, so a project moved to off runs every tree, drawn or not, as it ran before the block
// existed. A daemon with no workflow (no reader) records no tree and launches nothing with memory.
func TestMemoryReachesNoLaunchWithTheModeOffOrNoWorkflow(t *testing.T) {
	asked := false
	off := memorySpecs(t, config.MemoryOff, "/state/memory-instructions.md", false)
	off.memoryGroup = func(context.Context, string) (bool, error) {
		asked = true
		return true, nil
	}
	spec := memorySpawn(t, off, "LEGION-1", "LEGION-2", claim.RoleTester)
	if spec.Memory || spec.Prompt.MemoryInstructionsPath != "" {
		t.Errorf("mode off: Memory = %v, instructions = %q; want false and \"\"", spec.Memory, spec.Prompt.MemoryInstructionsPath)
	}
	if asked {
		t.Error("mode off asked for the tree's memory group")
	}

	none := memorySpecs(t, config.MemoryOn, "/state/memory-instructions.md", false)
	spec = memorySpawn(t, none, "LEGION-1", "LEGION-2", claim.RoleTester)
	if spec.Memory || spec.Prompt.MemoryInstructionsPath != "" {
		t.Errorf("no workflow: Memory = %v, instructions = %q; want false and \"\"", spec.Memory, spec.Prompt.MemoryInstructionsPath)
	}
}

// `mode: on` with no instructions file is allowed: the tree's roles get the overlays and nothing is
// appended to their prompt. A reader that cannot answer refuses the launch rather than running it
// as a tree with no memory.
func TestMemoryWithoutInstructionsAppendsNothingAndAFailedReadRefuses(t *testing.T) {
	on := memorySpecs(t, config.MemoryOn, "", true)
	on.memory.InstructionsPath = ""
	spec := memorySpawn(t, on, "LEGION-1", "LEGION-2", claim.RoleTester)
	if !spec.Memory || spec.Prompt.MemoryInstructionsPath != "" {
		t.Errorf("a block with no instructions: Memory = %v, instructions = %q; want true and \"\"", spec.Memory, spec.Prompt.MemoryInstructionsPath)
	}

	refused := memorySpecs(t, config.MemoryOn, "/state/memory-instructions.md", true)
	refused.memoryGroup = func(context.Context, string) (bool, error) { return false, errors.New("the store is down") }
	token, err := claim.NewToken("test", "LEGION-2", claim.RoleTester)
	if err != nil {
		t.Fatal(err)
	}
	_, err = refused.SpawnSpec(context.Background(), supervise.Claim{Token: token, Project: "test", Issue: "LEGION-2", Tree: "LEGION-1", Role: claim.RoleTester})
	want := "the memory group of tree LEGION-1: the store is down"
	if err == nil || err.Error() != want {
		t.Fatalf("SpawnSpec over an unreadable record = %v, want %q", err, want)
	}
}

// The boot read of a project's memory overlays refuses a file no pane could read, naming the key
// and the file, as role_config_files' own read refuses one; a project at mode off passes its keys
// on to no pane, so its files are left alone.
func TestMemoryOverlaysNoPaneCouldReadRefuseTheBoot(t *testing.T) {
	cfg := memoryConfig(config.MemoryOn)
	cfg.Projects["test"] = config.Project{Memory: config.Memory{Mode: config.MemoryOn, ConfigFiles: []string{"/etc/legion-memory/absent.yml"}}}
	err := checkRoleConfigFiles(cfg)
	want := "projects.test.memory.config_files names /etc/legion-memory/absent.yml, which a memory-group role's Oh My Pi could not read: "
	if err == nil || len(err.Error()) < len(want) || err.Error()[:len(want)] != want {
		t.Fatalf("checkRoleConfigFiles = %v, want it to start %q", err, want)
	}

	off := memoryConfig(config.MemoryOff)
	off.Projects["test"] = config.Project{Memory: config.Memory{Mode: config.MemoryOff, ConfigFiles: []string{"/etc/legion-memory/absent.yml"}}}
	if err := checkRoleConfigFiles(off); err != nil {
		t.Errorf("checkRoleConfigFiles at mode off = %v, want nil", err)
	}
}

// The tmux boot gate reads each workflow role's whole PI_CONFIG_FILES, the project's memory
// overlays after that role's own, so a memory overlay Oh My Pi refuses beside a role's own file
// refuses the boot naming both keys, rather than failing that role's every launch. The controller
// is in no memory group, so its value is its own overlays alone and a memory overlay it would
// refuse stops nothing; a project at mode off is probed as it was before the block existed.
func TestTheTmuxBootGateReadsAMemoryRolesWholeValue(t *testing.T) {
	dir := t.TempDir()
	omp := filepath.Join(dir, "omp")
	// The stub refuses exactly the pair: a value naming both the controller's file and a memory
	// overlay, which no pane is ever given, so only the gate's own composition can produce it.
	script := "#!/bin/sh\ncase \"$PI_CONFIG_FILES\" in *controller.yml*memory.yml*)\n" +
		"  echo \"error: Failed to parse config overlay $PI_CONFIG_FILES: SyntaxError\" >&2; exit 1;; esac\n" +
		"echo '{\"value\": false}'\n"
	if err := os.WriteFile(omp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	controller, memory := filepath.Join(dir, "controller.yml"), filepath.Join(dir, "memory.yml")
	for _, file := range []string{controller, memory} {
		if err := os.WriteFile(file, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	image := capabilities.Image{Launch: omp, Env: []string{"PATH=" + os.Getenv("PATH")}, WorkDir: dir}
	cfg := config.Config{
		Project:         "test",
		Runtime:         config.Runtime{Name: "tmux"},
		RoleConfigFiles: map[claim.Role][]string{claim.RoleController: {controller}},
		Projects: map[string]config.Project{"test": {
			Memory: config.Memory{Mode: config.MemoryOn, ConfigFiles: []string{memory}},
		}},
	}
	if err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger()); err != nil {
		t.Fatalf("checkPaneRoleOverlays = %v, want nil: the controller is in no memory group, so it is never given both", err)
	}

	// The same memory overlay given to a role that also names the file the stub refuses it beside.
	cfg.RoleConfigFiles = map[claim.Role][]string{claim.RoleTester: {controller}}
	err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger())
	want := "boot gate: role_config_files.tester with projects.test.memory.config_files: the tester's Oh My Pi would not start: "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("checkPaneRoleOverlays = %v, want it to start %q", err, want)
	}

	// At mode off the memory overlays reach no pane, so the gate reads the role's own alone.
	cfg.Projects["test"] = config.Project{Memory: config.Memory{Mode: config.MemoryOff, ConfigFiles: []string{memory}}}
	if err := checkPaneRoleOverlays(context.Background(), cfg, image, quietLogger()); err != nil {
		t.Fatalf("checkPaneRoleOverlays at mode off = %v, want nil", err)
	}
}
