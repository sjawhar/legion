package tmux

import (
	"reflect"
	"testing"
)

// Every argv the runtime hands tmux, against the argv the shipped TypeScript builds for the same
// step — the shipped file and line on each row. Where the Go runtime departs from the shipped set
// it is on purpose, and said here: the two orphan listings add `-u` (so their tab separators
// survive a daemon whose environment carries no UTF-8 locale) and read `window_activity` for a pane
// (tmux 3.7c has no `pane_activity`: it expands to nothing); a pane's command is `/bin/sh -c <shell
// command>` rather than one word tmux hands to the default shell; and the session is not marked
// with @legion_owner (tmux.ts:298) — a window's `#{@legion_owner}` falls back to its session's, so
// that marker made a human's window in the session read as the daemon's
// (TestRealTmuxReconcileOrphans).
func TestArgvMatchesTheShippedStrings(t *testing.T) {
	const socket = "legion-omp"
	pane := []string{"-e", "LEGION_TREE=LEGION-42", "/bin/sh", "-c", "cd /w && exec omp"}
	for _, tc := range []struct {
		name string
		got  []string
		want []string
	}{
		{
			// tmux.ts:279, and processes.test.ts:1047's recorded argv.
			name: "has-session",
			got:  hasSessionArgv(socket, "legion-omp"),
			want: []string{"tmux", "-L", "legion-omp", "has-session", "-t", "legion-omp"},
		},
		{
			// tmux.ts:281-294 with disableEnvironmentUpdatesArgs (:170-172): one invocation, `;` as
			// its own element (tmux.test.ts:416-433, processes.test.ts:1048-1065).
			name: "new-session with update-environment emptied in the same invocation",
			got:  newSessionArgv(socket, "legion-omp"),
			want: []string{
				"tmux", "-L", "legion-omp", "new-session", "-d", "-s", "legion-omp", "-n",
				"__legion_bootstrap", "sleep 3600", ";", "set-option", "-t", "legion-omp",
				"update-environment", "",
			},
		},
		{
			// tmux.ts:203-213 (window scope), processes.test.ts:1129.
			name: "window owner marker",
			got:  markWindowOwnerArgv(socket, "@42", "legion-omp"),
			want: []string{"tmux", "-L", "legion-omp", "set-option", "-w", "-t", "@42", "@legion_owner", "legion-omp"},
		},
		{
			// tmux.ts:326-338, processes.test.ts:1067-1080.
			name: "new-window",
			got:  newWindowArgv(socket, "legion-omp", "legion-42", pane),
			want: append([]string{
				"tmux", "-L", "legion-omp", "new-window", "-d", "-P", "-F",
				"#{window_id} #{pane_id} #{pane_pid}", "-t", "legion-omp", "-n", "legion-42",
			}, pane...),
		},
		{
			// tmux.ts:341-343, processes.test.ts:1128.
			name: "bootstrap window cleanup",
			got:  killBootstrapWindowArgv(socket, "legion-omp"),
			want: []string{"tmux", "-L", "legion-omp", "kill-window", "-t", "legion-omp:__legion_bootstrap"},
		},
		{
			// tmux.ts:366-377.
			name: "split-window",
			got:  splitWindowArgv(socket, "@42", pane),
			want: append([]string{
				"tmux", "-L", "legion-omp", "split-window", "-t", "@42", "-P", "-F", "#{pane_id} #{pane_pid}",
			}, pane...),
		},
		{
			// tmux.ts:382.
			name: "select-layout after a split",
			got:  selectLayoutArgv(socket, "@42"),
			want: []string{"tmux", "-L", "legion-omp", "select-layout", "-t", "@42", "tiled"},
		},
		{
			// tmux.ts:409-411 (lookupPane).
			name: "list-panes for one pane's window",
			got:  listPaneArgv(socket, "%1533"),
			want: []string{"tmux", "-L", "legion-omp", "list-panes", "-t", "%1533", "-F", "#{pane_id} #{pane_pid}"},
		},
		{
			// tmux.ts:445.
			name: "kill-pane",
			got:  killPaneArgv(socket, "%1533"),
			want: []string{"tmux", "-L", "legion-omp", "kill-pane", "-t", "%1533"},
		},
		{
			// tmux.ts:436.
			name: "kill-window",
			got:  killWindowArgv(socket, "@42"),
			want: []string{"tmux", "-L", "legion-omp", "kill-window", "-t", "@42"},
		},
		{
			// tmux.ts:466-475, plus -u.
			name: "orphan listing: owned windows",
			got:  listOwnedWindowsArgv(socket, "legion-omp"),
			want: []string{
				"tmux", "-u", "-L", "legion-omp", "list-windows", "-t", "legion-omp", "-F",
				"#{window_id}\t#{@legion_owner}\t#{window_activity}",
			},
		},
		{
			// tmux.ts:516-524, plus -u, window_activity for pane_activity.
			name: "orphan listing: every pane",
			got:  listAllPanesArgv(socket),
			want: []string{
				"tmux", "-u", "-L", "legion-omp", "list-panes", "-a", "-F",
				"#{pane_id}\t#{window_id}\t#{@legion_owner}\t#{pane_start_command}\t#{window_activity}",
			},
		},
		{
			// tmux.ts:131-132, tmux.test.ts:265.
			name: "show-environment, the global table",
			got:  showEnvironmentArgv(socket, globalTable),
			want: []string{"tmux", "-L", "legion-omp", "show-environment", "-s", "-g"},
		},
		{
			// tmux.ts:131-132, tmux.test.ts:266.
			name: "show-environment, the session table",
			got:  showEnvironmentArgv(socket, sessionTable("legion-omp")),
			want: []string{"tmux", "-L", "legion-omp", "show-environment", "-s", "-t", "legion-omp"},
		},
		{
			// tmux.ts:156-157, tmux.test.ts:339.
			name: "set-environment -u, the global table",
			got:  unsetEnvironmentArgv(socket, globalTable, "FOO_SECRET"),
			want: []string{"tmux", "-L", "legion-omp", "set-environment", "-g", "-u", "FOO_SECRET"},
		},
		{
			// tmux.ts:156-157, tmux.test.ts:340.
			name: "set-environment -u, the session table",
			got:  unsetEnvironmentArgv(socket, sessionTable("legion-omp"), "SSH_AUTH_SOCK"),
			want: []string{"tmux", "-L", "legion-omp", "set-environment", "-t", "legion-omp", "-u", "SSH_AUTH_SOCK"},
		},
		{
			// tmux.ts:142-147 (nameToken), tmux.test.ts:348: tmux strips a trailing `;` from a token.
			name: "set-environment -u of a name ending in ;",
			got:  unsetEnvironmentArgv(socket, globalTable, "HOME;"),
			want: []string{"tmux", "-L", "legion-omp", "set-environment", "-g", "-u", `HOME\;`},
		},
		{
			// tmux.ts:170-172, tmux.test.ts:370.
			name: "update-environment emptied on an existing session",
			got:  disableEnvironmentUpdatesArgv(socket, "legion-omp"),
			want: []string{"tmux", "-L", "legion-omp", "set-option", "-t", "legion-omp", "update-environment", ""},
		},
		{
			// runtime-tmux.ts:599-600 puts the shell command in one word after the -e pairs, which
			// tmux runs through the default shell; here the shell is named, so it is `sh -c`
			// whatever the operator's SHELL is, and never a login shell.
			name: "the pane's command",
			got:  paneCommand([]string{"-e", "A=1"}, "cd /w && exec omp"),
			want: []string{"-e", "A=1", "/bin/sh", "-c", "cd /w && exec omp"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Errorf("argv =\n%q\nwant\n%q", tc.got, tc.want)
			}
		})
	}
}
