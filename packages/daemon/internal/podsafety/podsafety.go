// Package podsafety is the baseline a Legion pod's Oh My Pi starts with, and nothing in it names a
// model, a provider, or a route: a settings overlay that holds off the endpoints Oh My Pi posts a
// conversation to on its own and keeps a long-running bash call inside the turn an abort can still
// reach (supervise.Machine.Quiesce depends on this), and four variables through which a
// repository's .env would move what the agent runs with. The overlay yields to the operator's own
// (runtime.kubernetes.pod): it is named first, before the operator's overlays. Apply keeps a
// variable the pod's environment sets, and the operator may set OTEL_SDK_DISABLED and PI_AUTO_QA;
// the Sandbox runtime refuses
// PI_CONFIG_DIR and OMP_SESSION_STORAGE in the operator's pod, since they decide where a session
// lives, and a pod keeps its sessions as files on the tree volume (sandbox.CheckPod). Apply, the
// full baseline, runs only in a pod (`legion worker-shim --pod-safety`, `legion probe-image
// --pod-safety`); a pane gets the two turn-scoping keys alone, as TurnScopeOverlay, which
// runtime/tmux writes and names itself (writeTurnScopeOverlay, panePairs).
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

// overlay is the settings overlay Apply writes (overlay.yml, which says what each setting holds off).
//
//go:embed overlay.yml
var overlay []byte

// OverlayFile is the overlay's file under the state directory Apply is given.
const OverlayFile = "podsafety-overlay.yml"

// TurnScopeOverlay is the two turn-scoping keys every Legion role's Oh My Pi needs regardless of
// runtime (turnscope.yml, which says why): bash.autoBackground and async, both off, without the
// rest of the pod baseline. The pod overlay above already carries both; a runtime with no overlay
// mechanism of its own (runtime/tmux) writes this one directly, named first in a pane's
// PI_CONFIG_FILES.
//
//go:embed turnscope.yml
var TurnScopeOverlay []byte

// TurnScopeFile is TurnScopeOverlay's file name, wherever a runtime writes it under its own state
// directory.
const TurnScopeFile = "podsafety-turnscope-overlay.yml"

// settingsOverlays is Oh My Pi's list of settings overlays, PATH-separated: each outranks the
// repository's .omp/config.yml, and a later one outranks an earlier one.
const settingsOverlays = "PI_CONFIG_FILES"

// placesSessions is why an operator's pod may not set a variable that decides where Oh My Pi keeps
// a session: a pod keeps its sessions as files on the tree volume, where a resume reads them.
const placesSessions = "it decides where Oh My Pi keeps the session a resume reads"

// baseline are the variables Apply sets where the pod's environment leaves them unset or empty,
// which is when Oh My Pi would fill them from the working directory's .env, each with why an
// operator's pod may not set it instead (Variable.Reserved), when it may not.
var baseline = []struct{ name, value, reserved string }{
	// The OpenTelemetry SDK exports logs, traces, and metrics to OTEL_EXPORTER_OTLP_ENDPOINT.
	{"OTEL_SDK_DISABLED", "true", ""},
	// Outranks dev.autoqa, which pushes tool-issue reports.
	{"PI_AUTO_QA", "0", ""},
	// Names the config root Oh My Pi's agent directory is joined under (.omp in the image), which
	// chooses the models.yml and profile it reads, and the sessions under it: no settings overlay
	// outranks it.
	{"PI_CONFIG_DIR", ".omp", placesSessions},
	// Outranks session.storage, and with OMP_SESSION_SQL_DSN_FILE would write the conversation to
	// a database the repository names; `file` keeps each session a file.
	{"OMP_SESSION_STORAGE", "file", placesSessions},
}

// Variable is one variable Apply sets. Reserved, when set, is why an operator's pod may not set it
// itself: Apply yields to the pod's own value, which for such a variable would give away something
// the runtime relies on. The operator may set every other one, and the operator's value is kept.
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

// Apply is environ as a pod's Oh My Pi starts with it: the overlay written read-only to
// <stateDir>/podsafety-overlay.yml and named first in PI_CONFIG_FILES, ahead of the overlays
// environ already names, and each baseline variable set where environ leaves it unset or empty.
func Apply(environ []string, stateDir string) ([]string, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("pod safety: no state directory to write %s to", OverlayFile)
	}
	file := filepath.Join(stateDir, OverlayFile)
	if err := WriteReadOnly(file, overlay); err != nil {
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
// fall back to the config root (`$HOME/$PI_CONFIG_DIR/profiles/<profile>`, as ompdirs.ProfileRoot
// ports it), and a state home the pod set to keep one role's browser broker lock apart from its
// siblings' (sandbox.roleStateHome) changes nothing. The profile is OMP_PROFILE where environ
// defines it, else PI_PROFILE, as Oh My Pi reads them (ompdirs.NormalizeProfile); a name Oh My Pi
// would refuse is refused here. With XDG_STATE_HOME unset or empty nothing is made: Oh My Pi reads
// no state home then.
func EnsureStateHome(environ []string) error {
	env := lookup(environ)
	stateHome := env["XDG_STATE_HOME"]
	if stateHome == "" {
		return nil
	}
	requested, set := env["OMP_PROFILE"]
	if !set {
		requested = env["PI_PROFILE"]
	}
	profile, valid := ompdirs.NormalizeProfile(requested)
	if !valid {
		return fmt.Errorf("pod safety: Oh My Pi refuses the profile %q; no state directory to make under %s", requested, stateHome)
	}
	dir := filepath.Join(stateHome, "omp")
	if profile != "" {
		dir = filepath.Join(dir, "profiles", profile)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pod safety: make Oh My Pi's state directory %s: %w", dir, err)
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
