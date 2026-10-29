package runtime

import (
	"regexp"
	"slices"
)

// sessionPlacing are the variables that decide where Oh My Pi keeps the session a resume reads:
// its config root and agent directory, under which a file session lives (PI_CONFIG_DIR,
// PI_CODING_AGENT_DIR), its session store (OMP_SESSION_STORAGE), and the session database's URL,
// by pointer or by value (OMP_SESSION_SQL_DSN_FILE, OMP_SESSION_SQL_DSN). The Sandbox runtime
// refuses the same names in an operator's pod, from ompsessions' and podsafety's constants
// (sandbox.CheckPod), and TestPaneVariableRefusalCoversTheSandboxRuntimesSessionNames holds this
// list to those constants: ompsessions imports this package, so it cannot name them here.
var sessionPlacing = []string{"OMP_SESSION_SQL_DSN", "OMP_SESSION_SQL_DSN_FILE", "OMP_SESSION_STORAGE", "PI_CODING_AGENT_DIR", "PI_CONFIG_DIR"}

// ClaudeConfigDirVariable is the variable a Claude Code worker's own configuration directory is
// named in, under either runtime (claudelaunch.Environment): the claim's session transcripts, MCP
// configuration and bridge state (tmux.ClaudeConfigDir).
const ClaudeConfigDirVariable = "CLAUDE_CONFIG_DIR"

// ClaudeDisabledFeatures are the variables every Claude Code worker carries set to 1, under either
// runtime (claudelaunch.Environment), to turn off what would run after a turn's end unseen:
// background tasks and scheduled prompts.
var ClaudeDisabledFeatures = []string{"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "CLAUDE_CODE_DISABLE_CRON"}

// launchSecrets are every secret the daemon hands a launch in its spec (daemon.launchSecrets: the
// Envoy bearer and the pane NATS nkey seed, each when the daemon has one), which a runtime gives
// the agent as a file of the launch's own behind a `<NAME>_FILE` pointer it sets per launch.
var launchSecrets = []string{"ENVOY_TOKEN", "NATS_NKEY_SEED"}

// urlPassword is a URL whose userinfo carries a password, `scheme://user:password@`, anywhere in
// a value: a database URL, a NATS URL with credentials, one of a comma-separated list of either.
var urlPassword = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/?#@:]*:[^\s/?#@]+@`)

// PaneVariableRefusal is why the operator may not add name, set to value, to every pane of the tmux
// runtime (`runtime.tmux.pane_env`), or "" when nothing here refuses it. The loader asks it at load
// (config) and the tmux runtime again at boot (tmux.New), each beside its own checks of what only
// it can see. A variable that places the session a resume reads is refused, since the runtime
// places a pane's sessions itself and a resume must find the session where the agent left it; so
// is one the runtime sets on every Claude Code pane after the operator's variables, where the
// operator's value would be overridden without a word, and a launch secret's pointer, which every
// launch sets to a file of its own and the runtime then refuses at every launch. A value that is a
// URL carrying a password is refused too: a pane variable is a value on tmux's command line, which
// every process on the host can read, and a secret reaches a pane as a file. The reason names the
// variable's role and never the value.
func PaneVariableRefusal(name, value string) string {
	switch {
	case slices.Contains(sessionPlacing, name):
		return "it decides where Oh My Pi keeps the session a resume reads, which the runtime places itself"
	case name == ClaudeConfigDirVariable || slices.Contains(ClaudeDisabledFeatures, name):
		return "the runtime sets it on every Claude Code pane itself, after the operator's variables"
	}
	for _, secret := range launchSecrets {
		if name == secret+"_FILE" {
			return "it points at the launch secret " + secret + ", which the runtime hands every launch as a file of the launch's own"
		}
	}
	if urlPassword.MatchString(value) {
		return "its value is a URL carrying a password: a secret reaches a pane as a file (provider_keys), never as a value on tmux's command line"
	}
	return ""
}
