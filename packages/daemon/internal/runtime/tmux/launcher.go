package tmux

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Launcher is how a pane runs one harness (runtime.Backends), as boot resolved it: the command the
// pane's `legion worker-shim` runs after `--`, and how a live pane running it is told from any other
// process.
type Launcher interface {
	// Launch is spec's inner command and the -e pairs its pane carries beside every pane's, with
	// whatever the harness keeps under the state directory for the claim made.
	Launch(spec runtime.SpawnSpec, host Host) (inner string, pairs []string, err error)
	// Marker is what the command line of a pane's process holds while the pane runs this harness:
	// the last tier of verifyPane (runsAgent).
	Marker() string
}

// Host is what a launch's command names of the daemon's host: its state directory and the `legion`
// binary every pane runs.
type Host struct {
	StateDir, Legion string
}

// NewOmpLauncher is Oh My Pi's launch: the configured omp_launch_prefix and the invocation boot
// resolved (omplaunch.ResolveInvocation).
func NewOmpLauncher(prefix []string, invocation string) Launcher {
	return ompLauncher{prefix: prefix, invocation: invocation}
}

type ompLauncher struct {
	prefix     []string
	invocation string
}

// Launch is innerCommand; an Oh My Pi pane carries no pairs of its own.
func (l ompLauncher) Launch(spec runtime.SpawnSpec, _ Host) (string, []string, error) {
	return innerCommand(l.prefix, l.invocation, spec.ResumeSessionFile, spec.Prompt), nil, nil
}

// Marker is "omp", which the invocation in an Oh My Pi pane's command line names.
func (ompLauncher) Marker() string { return "omp" }

// refuseAnotherBackendsSession refuses to resume a launch on backend from a recorded session kept
// in another backend's session directory (runtime.BackendInfo.SessionDir): one recorded before the
// role's backend changed, which this backend cannot continue.
func refuseAnotherBackendsSession(stateDir string, backend runtime.Backend, file string) error {
	for other, info := range runtime.Backends {
		if other == backend || info.SessionDir == "" {
			continue
		}
		dir := filepath.Join(stateDir, info.SessionDir)
		if strings.HasPrefix(filepath.Clean(file), dir+string(filepath.Separator)) {
			return fmt.Errorf("the recorded session %s is a %s session under %s: the role's backend changed since it was recorded, and %s cannot continue it",
				file, info.Name, dir, runtime.Backends[backend].Name)
		}
	}
	return nil
}
