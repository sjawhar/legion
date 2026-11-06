// Package podsafety is what a Legion pod's Oh My Pi starts with beyond the operator's pod, and
// nothing in it names a model, a provider, or a route, nor holds a repository's settings off. It is
// three things. The baseline variables: the two that decide where a pod's Oh My Pi keeps its
// sessions (baseline), set on the agent where the pod leaves them unset — when Oh My Pi would
// otherwise fill them from the working directory's .env — and never on the shim; the Sandbox
// runtime refuses them, and OMP_SESSION_SQL_DSN_FILE, in the operator's pod (sandbox.CheckPod).
// Every session of a workflow role is a row in the database the deployment must name
// (runtime.kubernetes.session_store postgres or cloudsql-iam): the runtime starts every such
// generation with OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE itself, and Apply keeps
// both. The baseline's `file` is left for the one
// container the runtime tells neither, the image probe's, whose session nothing resumes — neither
// the probe's pod nor an issue's mounts a disk that outlives it, an issue pod's being the pod's
// own ephemeral volume (the project controller's pod does mount one its Sandbox owns, and it is
// told the database like every other role). The state-home
// directory: the one Oh My Pi's state root resolves to under the role's XDG_STATE_HOME
// (EnsureStateHome), made before Oh My Pi starts, since Oh My Pi reads the variable only where
// that directory exists. And the scratch directories: each directory the runtime tells a role to
// write what it writes without being told where — the temporary directory, the cache and data
// homes, bun's install cache and the two Go caches (EnsureScratch) — made before Oh My Pi
// starts, since a role told a temporary directory nothing created cannot run. Apply, the baseline
// alone, runs only in a pod, for a worker's shim (`legion worker-shim
// --pod-safety`) and for the image probe (`legion probe-image --pod-safety`), which certifies the
// baseline a worker's Oh My Pi starts on. The directories are the shim's alone: every role the
// shim starts is told a state home of its own (sandbox.roleStateHome), which in an issue pod keeps
// a role's browser broker lock apart from its siblings', and the issue's scratch on the issue's
// volume, which holds all of those but the two Go caches beside it and which every role of the
// pod is told the same of, while the probe pod runs one container, on the image's own
// directories, and is told neither.
//
// Legion writes no settings overlay of its own. A role's PI_CONFIG_FILES is the operator's own
// overlays with the role's own after them, composed by the runtime that starts the role
// (sandbox.roleOperatorEnv in a pod, tmux.panePairs in a pane), and Apply passes the value the pod
// carries through untouched: a repository's settings reach a pod's agent as they reach any agent
// session, under the operator's, and nothing of Legion's outranks either.
package podsafety

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ompdirs"
)

// placesSessions is why an operator's pod may not set a variable that decides where Oh My Pi keeps
// a session: Legion places a pod's sessions where a resume reads them, which under the Kubernetes
// runtime is the session database and nothing else, a pod's disk dying with the pod (INFRA-80).
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

// Variable is one variable Apply sets, with Reserved: why an operator's pod may not set it itself,
// since Apply yields to the pod's own value and such a value would give away something the runtime
// relies on. Every variable Variables publishes carries one.
type Variable struct {
	Name, Reserved string
}

// Variables are the variables Apply sets: each baseline variable, with its reason.
func Variables() []Variable {
	variables := make([]Variable, 0, len(baseline))
	for _, v := range baseline {
		variables = append(variables, Variable{Name: v.name, Reserved: v.reserved})
	}
	return variables
}

// Apply is environ as a pod's Oh My Pi starts with it: each baseline variable set where environ
// leaves it unset or empty, and nothing else. PI_CONFIG_FILES rides through as the pod carries it,
// the operator's overlays and the role's own, already composed by the runtime that starts the
// role, and no file is written anywhere, since Legion names no settings overlay of its own.
func Apply(environ []string) []string {
	out := make([]string, 0, len(environ)+len(baseline))
	set := map[string]bool{}
	for _, pair := range environ {
		name, value, _ := strings.Cut(pair, "=")
		if value != "" {
			set[name] = true
		}
		out = append(out, pair)
	}
	for _, v := range baseline {
		if !set[v.name] {
			out = append(removed(out, v.name), v.name+"="+v.value)
		}
	}
	return out
}

// removed is environ without its entries for name.
func removed(environ []string, name string) []string {
	return slices.DeleteFunc(environ, func(pair string) bool { return strings.HasPrefix(pair, name+"=") })
}

// CheckOverlay refuses file, a settings overlay a role's PI_CONFIG_FILES names, unless it is a
// regular file this process can open, as the role's Oh My Pi must: Oh My Pi refuses to start on an
// overlay it cannot read ("Config overlay not found"). A symbolic link is followed, since a
// ConfigMap's file is one.
func CheckOverlay(file string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", file)
	}
	opened, err := os.Open(file)
	if err != nil {
		return err
	}
	return opened.Close()
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

// ScratchVariables are the variables EnsureScratch makes the directory of, in the order it makes
// them: what sandbox.roleEnvironment tells a role to write in without being told where,
// wherever on the issue's volume the runtime puts them — the issue's scratch for the temporary
// directory, the cache and data homes and bun's install cache, the pair beside it for the two Go
// caches, every one of them the same for every role of the pod — less XDG_STATE_HOME, which is
// the role's own and EnsureStateHome's. The runtime is held to this list rather than to a copy
// of it (sandbox.TestTheRuntimeTellsTheShimEveryScratchDirectoryItMakes), so a directory the
// runtime starts naming that the shim would never make is a test failure, not a role whose
// temporary directory does not exist.
var ScratchVariables = []string{"TMPDIR", "XDG_CACHE_HOME", "XDG_DATA_HOME", "GOCACHE", "GOMODCACHE", "BUN_INSTALL_CACHE_DIR"}

// EnsureScratch makes every directory environ names in ScratchVariables — TMPDIR, XDG_CACHE_HOME,
// XDG_DATA_HOME, GOCACHE, GOMODCACHE and BUN_INSTALL_CACHE_DIR — with every parent, before Oh My
// Pi starts. A name environ leaves unset or empty, or sets to a relative path, makes nothing: the
// runtime always sets absolute ones (sandbox.roleEnvironment, on the issue's volume), a relative
// one is not the shim's to resolve, and making it under whatever working directory the pod
// happens to have would be worse than leaving the tool its own default. A directory that cannot
// be made is a refusal naming the variable and the path, so a role whose volume cannot be written
// never starts half-configured rather than part way through its first turn. XDG_STATE_HOME is not
// here: EnsureStateHome keeps that one, since it makes Oh My Pi's profile directory under the
// state home rather than the state home itself, and its refusal carries its own reason.
//
// `go` and `bun` make their own cache directories, so GOCACHE, GOMODCACHE and BUN_INSTALL_CACHE_DIR
// would come up by themselves; TMPDIR has no creator at all — mktemp and every library equivalent
// fail where the directory the variable names does not exist, and the pod's roles are
// told no other temporary directory. The list is the whole told set rather than that one name, so
// the shim makes what the runtime said it would use whatever the tool reading it does.
//
// The shim is what makes them, not the kubelet: a subPath mount per directory would have the
// kubelet create each one, but it would also contradict the rule that these paths sit on the issue
// volume's own mount (sandbox.TestTheIssuesRolesShareOneScratchAndOneGoCachePair), ask the
// kubelet for twelve directories — the four the issue's roles share, the two Go caches beside
// them and a state home for each of the issue pod's six roles — and rest on an unproven
// ownership behaviour: what mode a kubelet gives a nested sub-path directory it creates on a
// fresh volume. The shim runs as the pod's own user, which can write the volume, and makes
// exactly the directories the runtime told it to use, read from the same environment the agent
// starts on, so the names cannot drift.
func EnsureScratch(environ []string) error {
	env := lookup(environ)
	for _, name := range ScratchVariables {
		dir := env[name]
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("pod safety: make the directory %s names, %s: %w", name, dir, err)
		}
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
