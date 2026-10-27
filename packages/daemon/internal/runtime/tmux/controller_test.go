package tmux

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// controllerSpec is the launch of the project's controller the daemon launches itself
// (`controller: daemon`): no issue, no tree, no repository, under the project's controller token.
func controllerSpec(t *testing.T, project string) runtime.SpawnSpec {
	t.Helper()
	prompt := filepath.Join(t.TempDir(), "controller-root.md")
	if err := os.WriteFile(prompt, []byte("You are the stand-in controller.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime.SpawnSpec{
		Claim: claim.ControllerToken(project), Project: project, Role: claim.RoleController, Generation: 1, BootToken: "boot-secret",
		Env:    map[string]string{"TZ": "UTC"},
		Prompt: runtime.PromptParts{RolePromptPaths: []string{prompt}},
	}
}

// A controller pane is told the controller's marker in place of a tree pane's tree and issue, and
// the empty Slack channel list the plugin requires of a daemon-launched controller in place of the
// workspace: exactly the runtime's own names for a tree pane, less those three, plus the
// controller's two (controllerOwned), so a spec can override none of them.
func TestAControllerPaneIsToldTheControllerMarkerInPlaceOfATree(t *testing.T) {
	spec := controllerSpec(t, "omp")
	spec.Env = nil
	in := PaneInputs{
		StateDir: "/state", Workspace: "/state/controller", DaemonURL: "http://127.0.0.1:13370",
		EnvoyURL: "http://127.0.0.1:9020", NATSURLs: []string{"nats://a:4222"},
		DispatchURL: "http://127.0.0.1:18766", DispatchTokenFile: "/state/secrets/" + runtime.DispatchTokenFileName,
		GHOwnersDir: runtime.GHConfigDir("/state", spec.Claim), GHDefault: "acme", GH: "/usr/bin/gh",
	}
	pairs := panePairs(spec, in, secretFiles("/state", spec))
	told := map[string]string{}
	for i := 1; i < len(pairs); i += 2 {
		name, value, _ := strings.Cut(pairs[i], "=")
		told[name] = value
	}
	if told["LEGION_CONTROLLER"] != "1" {
		t.Errorf("LEGION_CONTROLLER = %q, want 1", told["LEGION_CONTROLLER"])
	}
	if value, ok := told["LEGION_SLACK_CHANNELS"]; !ok || value != "" {
		t.Errorf("LEGION_SLACK_CHANNELS = %q (set %v), want it set and empty", value, ok)
	}
	want := map[string]bool{}
	for name := range runtimeOwned {
		if name != "LEGION_TREE" && name != "LEGION_ISSUE" && name != "LEGION_WORKSPACE" {
			want[name] = true
		}
	}
	maps.Copy(want, controllerOwned)
	names := map[string]bool{}
	for name := range told {
		names[name] = true
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("a controller pane is told %v, want %v", slices.Sorted(maps.Keys(names)), slices.Sorted(maps.Keys(want)))
	}
}

// The controller works no checkout: its pane starts in <state_dir>/controller, made on the way, and
// opens a window of its own named for it, which no issue's window ever is (an issue key holds a `-`).
func TestTheControllerWorksInADirectoryAndAWindowOfItsOwn(t *testing.T) {
	f := newFakeServerRuntime(t, staticCredential, nil)
	spec := controllerSpec(t, "omp")
	if _, err := f.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn of the controller: %v", err)
	}
	dir := filepath.Join(f.stateDir, controllerWindow)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the controller's directory %s: %v", dir, err)
	}
	var opened []string
	for _, argv := range f.argvs {
		if verb(argv) == "new-window" {
			opened = argv
		}
	}
	if i := slices.Index(opened, "-n"); i < 0 || i+1 >= len(opened) || opened[i+1] != controllerWindow {
		t.Errorf("the controller's new-window = %q, want a window named %q", opened, controllerWindow)
	}
	if !slices.ContainsFunc(opened, func(word string) bool {
		return strings.Contains(word, "cd "+dir) || strings.Contains(word, "cd '"+dir+"'")
	}) {
		t.Errorf("the controller's pane command = %q, want it to start in %s", opened, dir)
	}
	pairs := f.pairs(t)
	for _, name := range []string{"LEGION_TREE=", "LEGION_ISSUE=", "LEGION_WORKSPACE="} {
		if slices.ContainsFunc(pairs, func(pair string) bool { return strings.HasPrefix(pair, name) }) {
			t.Errorf("the controller's pane was told %s…: %q", name, pairs)
		}
	}
	if !slices.Contains(pairs, "LEGION_CONTROLLER=1") {
		t.Errorf("the controller's pane was not told LEGION_CONTROLLER=1: %q", pairs)
	}
}

// A project with no GitHub owner to mint for (runtime.ErrNoGitHubOwner: no mapped repository has an
// App) still runs the controller the daemon launches, with gh acting as nobody, as `legion
// controller start` and the sandbox runtime do there: the launch succeeds, the controller's gh
// directory is an empty 0700 one — whatever an earlier launch left there removed — named by
// GH_CONFIG_DIR with the tokens emptied, and the refresher leaves it be without logging a failure.
// Any other role with no owner is still refused.
func TestAControllerWithNoGitHubOwnerRunsWithGhActingAsNobody(t *testing.T) {
	ctx := context.Background()
	var logs strings.Builder
	noOwner := func(_ context.Context, role claim.Role, _ string) (ghconfig.Bundle, error) {
		return ghconfig.Bundle{}, fmt.Errorf("the review App for %s: %w", role, runtime.ErrNoGitHubOwner)
	}
	f := newFakeServerRuntime(t, noOwner, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	spec := controllerSpec(t, "omp")
	root := runtime.GHConfigDir(f.stateDir, spec.Claim)
	stale, _ := staticCredential(ctx, claim.RoleController, "acme")
	if _, err := ghconfig.WriteBundle(root, stale); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Spawn(ctx, spec); err != nil {
		t.Fatalf("Spawn of a controller with no GitHub owner: %v", err)
	}
	if entries := ghDirEntries(t, root); len(entries) != 0 {
		t.Errorf("the controller's gh directory holds %q, want it empty", entries)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the controller's gh directory: %v, %v; want a 0700 directory", info, err)
	}
	pairs := f.pairs(t)
	for _, want := range []string{"GH_CONFIG_DIR=" + root, "GH_TOKEN=", "GITHUB_TOKEN=", "GH_HOST=", "LEGION_GH_OWNERS_DIR=" + root} {
		if !slices.Contains(pairs, want) {
			t.Errorf("the controller's pane was not told %q: %q", want, pairs)
		}
	}

	f.refreshGitHubCredentials(ctx)
	if failed := logLines(logs.String(), "github credential refresh failed"); len(failed) != 0 {
		t.Errorf("the refresher logged failures %q for a controller with no owner, want none", failed)
	}

	tester := f.spec(t, claim.RoleTester)
	if _, err := f.Spawn(ctx, tester); err == nil || !strings.Contains(err.Error(), runtime.ErrNoGitHubOwner.Error()) {
		t.Errorf("Spawn of a tester with no GitHub owner = %v, want the refusal naming it", err)
	}
}
