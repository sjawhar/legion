package tmux

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// tmuxOwnGlobals are the variables tmux itself writes into a server's global table when it starts
// (PWD from its cwd, SHLVL=0), whatever its client carried: present on a server this daemon forked,
// so never "left by an earlier daemon" (runtime-tmux.ts:36-39).
var tmuxOwnGlobals = map[string]bool{"PWD": true, "SHLVL": true}

// ScrubServerEnvironment removes, from the private server's two environment tables, every
// variable the pane environment does not carry, and returns the removed names sorted
// (runtime-tmux.ts:772-834). The server outlives the daemon, and each table reaches every pane
// opened afterwards beneath its -e pairs: the global table holds whatever environment the server
// was forked with — an earlier daemon's, or an operator's — and the session table whatever tmux's
// default `update-environment` copied from each attaching client (SSH_AUTH_SOCK, DISPLAY, …).
//
// The session's `update-environment` is emptied before its table is read, so an attach landing in
// between cannot slip a copy in behind the read. Names come from `show-environment -s` read whole
// (parseShellEnvironment), so a multi-line value is never mistaken for names. After the removals
// each table is read again and two invariants hold or this refuses, naming names only: every
// allow-listed name that was present is still present, and no removed name remains — the guard on
// the unset side's argv tokenizing (a name ending in `;` travels as `\;`).
//
// A server with no daemon session still has its global table scrubbed; no server at all is
// nothing to scrub. Panes already open keep their environment. Boot runs this once, before any
// pane opens.
func (r *Runtime) ScrubServerEnvironment(ctx context.Context) ([]string, error) {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()

	removed := map[string]bool{}
	globals, present, err := r.environmentNames(ctx, globalTable)
	if err != nil {
		return nil, err
	}
	if !present {
		return []string{}, nil
	}
	if err := r.scrubTable(ctx, globalTable, globals, removed); err != nil {
		return nil, err
	}
	session := sessionTable(r.socket)
	res, err := r.run(ctx, disableEnvironmentUpdatesArgv(r.socket, r.socket))
	if err != nil {
		return nil, err
	}
	switch {
	case res.exitCode == 0:
		names, present, err := r.environmentNames(ctx, session)
		if err != nil {
			return nil, err
		}
		if present {
			if err := r.scrubTable(ctx, session, names, removed); err != nil {
				return nil, err
			}
		}
	case !targetAbsent(res):
		return nil, fmt.Errorf("tmux set-option -t %s update-environment '' failed (exit %d)%s", r.socket, res.exitCode, failure(res))
	}
	out := make([]string, 0, len(removed))
	for name := range removed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// scrubTable removes every name of table outside the pane environment and tmux's own globals, then
// re-reads the table and checks both invariants.
func (r *Runtime) scrubTable(ctx context.Context, table envTable, names []string, removed map[string]bool) error {
	var kept, dropped []string
	for _, name := range names {
		if _, ok := r.paneEnv[name]; ok || tmuxOwnGlobals[name] {
			kept = append(kept, name)
			continue
		}
		res, err := r.run(ctx, unsetEnvironmentArgv(r.socket, table, name))
		if err != nil {
			return err
		}
		if res.exitCode != 0 {
			return fmt.Errorf("tmux set-environment %s -u %s failed (exit %d)%s",
				strings.Join(table.flags(), " "), name, res.exitCode, failure(res))
		}
		dropped = append(dropped, name)
		removed[name] = true
	}
	if len(dropped) == 0 {
		return nil
	}
	after, _, err := r.environmentNames(ctx, table)
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, name := range after {
		present[name] = true
	}
	var missing, lingering []string
	for _, name := range kept {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range dropped {
		if present[name] {
			lingering = append(lingering, name)
		}
	}
	if len(missing) == 0 && len(lingering) == 0 {
		return nil
	}
	message := fmt.Sprintf("tmux %s environment scrub did not land as intended", table)
	if len(missing) > 0 {
		message += "; allow-listed name(s) now missing: " + strings.Join(missing, ", ")
	}
	if len(lingering) > 0 {
		message += "; removed name(s) still present: " + strings.Join(lingering, ", ")
	}
	return fmt.Errorf("%s", message)
}

// environmentNames is the names in one table — present=false when there is nothing to read: no
// server on the socket, or no such session. Names only; the values are never kept, and a failure
// carries tmux's stderr, never its stdout, which is the value dump (tmux.ts:122-140).
func (r *Runtime) environmentNames(ctx context.Context, table envTable) (names []string, present bool, err error) {
	res, err := r.run(ctx, showEnvironmentArgv(r.socket, table))
	if err != nil {
		return nil, false, err
	}
	if res.exitCode != 0 {
		if targetAbsent(res) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("tmux show-environment -s %s failed (exit %d)%s",
			strings.Join(table.flags(), " "), res.exitCode, failure(res))
	}
	names, err = parseShellEnvironment(res.stdout, table)
	return names, true, err
}
