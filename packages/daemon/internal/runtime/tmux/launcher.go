package tmux

import (
	"slices"
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

// agentMarkers are the markers of launchers, sorted, and the harnesses they run as one phrase, in
// the order their names sort: what verifyPane accepts and what it says a pane is not running.
func agentMarkers(launchers map[runtime.Backend]Launcher) (markers []string, agents string) {
	var names []string
	for backend, launcher := range launchers {
		markers = append(markers, launcher.Marker())
		names = append(names, runtime.Backends[backend].Name)
	}
	slices.Sort(markers)
	slices.Sort(names)
	return slices.Compact(markers), strings.Join(names, " or ")
}
