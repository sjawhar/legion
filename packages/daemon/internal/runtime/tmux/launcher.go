package tmux

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
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
	// SessionsDir is the directory under which the harness, started for spec's claim in a pane
	// whose environment is env (the pane's: the daemon's allow-listed variables, the XDG base
	// directories under the state directory, and the operator's pane variables) in workDir, keeps
	// its sessions, one folder per working directory: where a resume looks for a recorded session
	// that was carried onto this host from elsewhere (sessionFileOf).
	SessionsDir(spec runtime.SpawnSpec, host Host, env map[string]string, workDir string) (string, error)
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

// SessionsDir is where Oh My Pi, started under env in workDir, keeps its sessions (getSessionsDir,
// @oh-my-pi/pi-utils src/dirs.ts): `sessions` under the agent directory, `<config root>/agent`,
// except where an XDG data root holds the profile (ompdirs.ProfileRoot), which flattens the
// `agent/` prefix to `$XDG_DATA_HOME/omp[/profiles/<name>]/sessions`. A pane's environment never
// holds PI_CODING_AGENT_DIR, which would move the agent directory itself: the daemon's allow-list
// does not carry it and runtime.PaneVariableRefusal refuses it as a pane variable. A launch prefix
// that sets a profile is not read here, as ompdirs says of its own reads.
func (ompLauncher) SessionsDir(_ runtime.SpawnSpec, _ Host, env map[string]string, workDir string) (string, error) {
	root, profile, err := ompdirs.ProfileRoot(env, workDir)
	if err != nil {
		return "", err
	}
	if data := env["XDG_DATA_HOME"]; data != "" {
		xdg := filepath.Join(data, "omp")
		if profile != "" {
			xdg = filepath.Join(xdg, "profiles", profile)
		}
		if root == xdg {
			return filepath.Join(root, "sessions"), nil
		}
	}
	return filepath.Join(root, "agent", "sessions"), nil
}

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
