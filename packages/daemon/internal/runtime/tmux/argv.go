package tmux

import "strings"

// Every argv the runtime hands tmux is built here, by a pure function, so the whole command
// vocabulary can be read — and tested against the shipped TypeScript's argv — in one place. Each
// starts `tmux -L <socket>`: the daemon's private server, forked by its own first command under
// the pane environment, never an operator's server (tmux.ts:8-16). The runner swaps `tmux` for
// the resolved binary.

// bootstrapWindow is the throwaway window a new session is created with, so the session can exist
// — and have `update-environment` emptied — before any real window runs in it (tmux.ts:190).
const bootstrapWindow = "__legion_bootstrap"

// ownerOption is the window option that marks a window as this daemon's, so reconciliation can
// tell it from one a human opened on the same server (tmux.ts:192-196).
const ownerOption = "@legion_owner"

// shell is what every pane's command runs under: named, so a pane's shell is `sh -c` whatever
// the operator's SHELL is, and never a login shell that would re-read a profile over the pane
// environment (LEGION-206 P1).
const shell = "/bin/sh"

// envTable names one of the two tmux environment tables a pane inherits beneath its -e pairs: the
// server's global table (the zero value) or one session's (tmux.ts:28-40).
type envTable struct{ session string }

var globalTable = envTable{}

func sessionTable(session string) envTable { return envTable{session: session} }

func (t envTable) flags() []string {
	if t.session == "" {
		return []string{"-g"}
	}
	return []string{"-t", t.session}
}

func (t envTable) String() string {
	if t.session == "" {
		return "global"
	}
	return "session " + t.session
}

func tmuxArgv(socket string, rest ...string) []string {
	return append([]string{"tmux", "-L", socket}, rest...)
}

// utf8Argv is tmuxArgv with `-u`, for a listing whose output is split on tabs: without it, tmux
// renders every control character — a tab included — as `_` to a client whose environment names
// no UTF-8 locale (tmux 3.7c, observed on the devbox), and a daemon started without LANG would
// parse nothing.
func utf8Argv(socket string, rest ...string) []string {
	return append([]string{"tmux", "-u", "-L", socket}, rest...)
}

func hasSessionArgv(socket, session string) []string {
	return tmuxArgv(socket, "has-session", "-t", session)
}

// newSessionArgv creates the session with its bootstrap window and empties its
// `update-environment` in the same client invocation — `;` as its own element is tmux's command
// separator — so no attach can copy a client's environment into the session in between
// (tmux.ts:262-300).
func newSessionArgv(socket, session string) []string {
	return tmuxArgv(socket, append(
		[]string{"new-session", "-d", "-s", session, "-n", bootstrapWindow, "sleep 3600", ";"},
		disableEnvironmentUpdates(session)...,
	)...)
}

func markWindowOwnerArgv(socket, windowID, owner string) []string {
	return tmuxArgv(socket, "set-option", "-w", "-t", windowID, ownerOption, owner)
}

// newWindowArgv opens a detached window and reports its window id, pane id, and pane pid from
// the same invocation, before the pane's command can exit (tmux.ts:302-357).
func newWindowArgv(socket, session, name string, pane []string) []string {
	return tmuxArgv(socket, append(
		[]string{"new-window", "-d", "-P", "-F", "#{window_id} #{pane_id} #{pane_pid}", "-t", session, "-n", name},
		pane...,
	)...)
}

func killBootstrapWindowArgv(socket, session string) []string {
	return tmuxArgv(socket, "kill-window", "-t", session+":"+bootstrapWindow)
}

// splitWindowArgv splits a pane into an existing window, reporting its pane id and pid from the
// same invocation (tmux.ts:359-384).
func splitWindowArgv(socket, windowID string, pane []string) []string {
	return tmuxArgv(socket, append(
		[]string{"split-window", "-t", windowID, "-P", "-F", "#{pane_id} #{pane_pid}"},
		pane...,
	)...)
}

func selectLayoutArgv(socket, windowID string) []string {
	return tmuxArgv(socket, "select-layout", "-t", windowID, "tiled")
}

// listPaneArgv lists the window a pane id resolves to; the pane-id column picks the row
// (tmux.ts:402-411).
func listPaneArgv(socket, paneID string) []string {
	return tmuxArgv(socket, "list-panes", "-t", paneID, "-F", "#{pane_id} #{pane_pid}")
}

func killPaneArgv(socket, paneID string) []string {
	return tmuxArgv(socket, "kill-pane", "-t", paneID)
}

func killWindowArgv(socket, windowID string) []string {
	return tmuxArgv(socket, "kill-window", "-t", windowID)
}

// listOwnedWindowsArgv lists the session's windows with their owner marker and last activity
// (tmux.ts:455-476).
func listOwnedWindowsArgv(socket, session string) []string {
	return utf8Argv(socket, "list-windows", "-t", session, "-F", "#{window_id}\t#{@legion_owner}\t#{window_activity}")
}

// listAllPanesArgv lists every pane on the server with its window, that window's owner marker,
// the command it was started with, and the window's last activity (tmux.ts:498-525). The shipped
// listing reads `pane_activity`, which tmux 3.7c does not define — it expands to nothing, so a
// pane's age read as zero and the grace never applied; the window's activity is the one clock
// tmux keeps for it.
func listAllPanesArgv(socket string) []string {
	return utf8Argv(socket, "list-panes", "-a", "-F",
		"#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}\t#{window_activity}")
}

// showEnvironmentArgv dumps one table in shell form, `NAME="value"; export NAME;` per entry
// (tmux.ts:122-140).
func showEnvironmentArgv(socket string, table envTable) []string {
	return tmuxArgv(socket, append([]string{"show-environment", "-s"}, table.flags()...)...)
}

// unsetEnvironmentArgv removes an entry from a table outright (-u), so no pane opened afterwards
// inherits it (tmux.ts:149-163).
func unsetEnvironmentArgv(socket string, table envTable, name string) []string {
	return tmuxArgv(socket, append(append([]string{"set-environment"}, table.flags()...), "-u", nameToken(name))...)
}

// nameToken is a variable name as a tmux argv token: tmux strips a trailing `;` from a token (its
// command separator), so `HOME;` would collapse onto `HOME`; `\;` is the literal (tmux.ts:142-147).
func nameToken(name string) string {
	if strings.HasSuffix(name, ";") {
		return strings.TrimSuffix(name, ";") + `\;`
	}
	return name
}

// disableEnvironmentUpdatesArgv empties an existing session's `update-environment`, so an
// attaching client no longer copies its SSH_AUTH_SOCK, DISPLAY, … into the table every later pane
// inherits (tmux.ts:165-188).
func disableEnvironmentUpdatesArgv(socket, session string) []string {
	return tmuxArgv(socket, disableEnvironmentUpdates(session)...)
}

func disableEnvironmentUpdates(session string) []string {
	return []string{"set-option", "-t", session, "update-environment", ""}
}

// paneCommand is what follows new-window's or split-window's own flags: the pane's -e pairs, then
// its shell command under `sh -c`. Passed as separate words, tmux executes them directly rather
// than handing one word to the default shell.
func paneCommand(pairs []string, shellCommand string) []string {
	return append(append([]string{}, pairs...), shell, "-c", shellCommand)
}
