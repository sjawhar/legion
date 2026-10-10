// Package podsafety is what a Legion pod's Oh My Pi starts with beyond the operator's pod, and
// nothing in it names a model, a provider, or a route, nor holds a repository's settings off. It is
// three things. The turn-scoping overlay: the two keys supervise.Machine.Quiesce depends on
// (TurnScopeOverlay, turnscope.yml, which says why), written as the one settings overlay Legion
// names first in PI_CONFIG_FILES, before the operator's own (runtime.kubernetes.pod): by Oh My Pi's
// order the operator's overlay outranks it, and both outrank a repository's .omp/config.yml, so a
// repository's settings reach a pod's agent as they reach any agent session, under the operator's.
// The baseline variables: the two that decide where a pod's Oh My Pi keeps its sessions
// (baseline), set on the agent where the pod leaves them unset — when Oh My Pi would otherwise fill
// them from the working directory's .env — and never on the shim; the Sandbox runtime refuses them,
// and OMP_SESSION_SQL_DSN_FILE, in the operator's pod (sandbox.CheckPod): a pod keeps its sessions
// as files on the issue's volume, where a resume reads them, unless the runtime keeps them in a
// database (runtime.kubernetes.session_store postgres), where it starts every generation with
// OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE itself, and Apply keeps both. And the
// state-home directory: the one Oh My Pi's state root resolves to under the role's XDG_STATE_HOME
// (EnsureStateHome), made before Oh My Pi starts, since Oh My Pi reads the variable only where that
// directory exists. Apply, the overlay and the baseline together, runs only in a pod, for a
// worker's shim (`legion worker-shim --pod-safety`) and for the image probe (`legion probe-image
// --pod-safety`), which certifies the baseline a worker's Oh My Pi starts on. The state home is the
// shim's alone: every role the shim starts is told one of its own (sandbox.roleStateHome), which in
// an issue pod keeps a role's browser broker lock apart from its siblings', and the probe pod runs
// one container and is told none. A pane gets the overlay alone, which runtime/tmux writes and
// names itself (writeTurnScopeOverlay, panePairs).
package podsafety

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
)

// TurnScopeOverlay is the one settings overlay every Legion role's Oh My Pi gets, pod or pane
// (turnscope.yml, which says why): bash.autoBackground and async, both off, and nothing else. Apply
// writes it for a pod; a runtime with no overlay mechanism of its own (runtime/tmux) writes it
// directly, named first in a pane's PI_CONFIG_FILES.
//
//go:embed turnscope.yml
var TurnScopeOverlay []byte

// TurnScopeFile is TurnScopeOverlay's file name under the state directory a runtime writes it to:
// Apply's stateDir in a pod, the daemon's own for a pane.
const TurnScopeFile = "podsafety-turnscope-overlay.yml"

// settingsOverlays is Oh My Pi's list of settings overlays, PATH-separated: each outranks the
// repository's .omp/config.yml, and a later one outranks an earlier one.
const settingsOverlays = "PI_CONFIG_FILES"

// placesSessions is why an operator's pod may not set a variable that decides where Oh My Pi keeps
// a session: Legion places a pod's sessions where a resume reads them, as files on the issue's
// volume or in the runtime's session database.
const placesSessions = "it decides where Oh My Pi keeps the session a resume reads"

// baseline are the variables Apply sets where the pod's environment leaves them unset or empty,
// which is when Oh My Pi would fill them from the working directory's .env: the two that place a
// pod's sessions, each with why an operator's pod may not set it instead (Variable.Reserved).
// Nothing else is set on a pod's agent; a repository's own settings and .env are its own otherwise.
var baseline = []struct{ name, value, reserved string }{
	// Names the config root Oh My Pi's agent directory is joined under (.omp in the image), which
	// chooses the models.yml and profile it reads, and the sessions under it: no settings overlay
	// outranks it.
	{"PI_CONFIG_DIR", ".omp", placesSessions},
	// Outranks session.storage, and with OMP_SESSION_SQL_DSN_FILE would write the conversation to
	// a database the repository names; `file` keeps each session a file. A runtime that keeps
	// sessions in its own database sets sql and the URL file's pointer before Apply runs.
	{"OMP_SESSION_STORAGE", "file", placesSessions},
}

// Variable is one variable Apply sets. Reserved, when set, is why an operator's pod may not set it
// itself: Apply yields to the pod's own value, which for such a variable would give away something
// the runtime relies on. The one other, PI_CONFIG_FILES, the operator names its own overlays in,
// and Apply composes.
type Variable struct {
	Name, Reserved string
}

// Variables are the variables Apply sets: the settings overlays it composes, ahead of the
// operator's, and each baseline variable.
func Variables() []Variable {
	variables := []Variable{{Name: settingsOverlays}}
	for _, v := range baseline {
		variables = append(variables, Variable{Name: v.name, Reserved: v.reserved})
	}
	return variables
}

// Apply is environ as a pod's Oh My Pi starts with it: TurnScopeOverlay written read-only to
// <stateDir>/<TurnScopeFile> and named first in PI_CONFIG_FILES, ahead of the overlays environ
// already names, and each baseline variable set where environ leaves it unset or empty.
func Apply(environ []string, stateDir string) ([]string, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("pod safety: no state directory to write %s to", TurnScopeFile)
	}
	file := filepath.Join(stateDir, TurnScopeFile)
	if err := WriteReadOnly(file, TurnScopeOverlay); err != nil {
		return nil, fmt.Errorf("pod safety: write %s: %w", file, err)
	}
	overlays := file
	out := make([]string, 0, len(environ)+len(baseline)+1)
	set := map[string]bool{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		switch {
		case name == settingsOverlays:
			if value != "" {
				overlays += string(os.PathListSeparator) + value
			}
			continue
		case value != "":
			set[name] = true
		}
		out = append(out, pair)
	}
	for _, v := range baseline {
		if !set[v.name] {
			out = append(removed(out, v.name), v.name+"="+v.value)
		}
	}
	return append(out, settingsOverlays+"="+overlays), nil
}

// removed is environ without its entries for name.
func removed(environ []string, name string) []string {
	return slices.DeleteFunc(environ, func(pair string) bool { return strings.HasPrefix(pair, name+"=") })
}

// EnsureStateHome makes the directory Oh My Pi's state root resolves to under environ's
// XDG_STATE_HOME, so the variable takes effect. Oh My Pi's state-class directories (`run/daemons`,
// `logs`, `browser-profiles`, `reports`, `terminal-sessions`; sessions are data-class) hang under
// `$XDG_STATE_HOME/omp/profiles/<profile>` for a named profile and `$XDG_STATE_HOME/omp` for the
// default one, but only when that directory already exists when Oh My Pi starts; otherwise they
// fall back to the config root (`$HOME/$PI_CONFIG_DIR/profiles/<profile>`, as ompdirs.StateRoot
// ports it), and a state home the pod set to keep one role's browser broker lock apart from its
// siblings' (sandbox.roleStateHome) changes nothing. The directory is the one ompdirs answers
// (ompdirs.StateRootCandidate), so the shim and the port cannot drift: the profile is OMP_PROFILE
// where environ defines it, else PI_PROFILE, as Oh My Pi reads them, and a name Oh My Pi would
// refuse is refused here. With XDG_STATE_HOME unset or empty nothing is made: Oh My Pi reads no
// state home then. Once made, ompdirs.StateRoot over environ, in the working directory the shim's
// Oh My Pi inherits, must answer that directory; where it answers another — an honoured
// PI_CODING_AGENT_DIR elsewhere turns Oh My Pi's XDG lookup off — this refuses, naming both, rather
// than let the shim make a directory Oh My Pi ignores while every role's broker takes one lock name.
func EnsureStateHome(environ []string) error {
	env := lookup(environ)
	dir, ok, err := ompdirs.StateRootCandidate(env)
	if err != nil {
		return fmt.Errorf("pod safety: no state directory to make under XDG_STATE_HOME=%s: %w", env["XDG_STATE_HOME"], err)
	}
	if !ok {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pod safety: make Oh My Pi's state directory %s: %w", dir, err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("pod safety: the working directory Oh My Pi starts in: %w", err)
	}
	root, _, err := ompdirs.StateRoot(env, workDir)
	if err != nil {
		return fmt.Errorf("pod safety: %w", err)
	}
	if root != dir {
		return fmt.Errorf("pod safety: Oh My Pi would keep its state under %s, not under %s, the directory made under XDG_STATE_HOME: the state home changes nothing (an honoured PI_CODING_AGENT_DIR elsewhere turns Oh My Pi's XDG lookup off)", root, dir)
	}
	return nil
}

// lookup is environ by name: a name environ defines is present, with its value, "" included.
func lookup(environ []string) map[string]string {
	env := make(map[string]string, len(environ))
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env
}

// WriteReadOnly writes body to file at mode 0444, through a temporary file renamed into place, so
// a file an earlier start left, read-only, is replaced rather than refusing the write.
func WriteReadOnly(file string, body []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o444); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), file)
}
