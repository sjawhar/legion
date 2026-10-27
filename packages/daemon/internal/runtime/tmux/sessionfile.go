package tmux

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// sessionFileOf is the file a resume continues the claim's recorded session from, and the
// sessions directories it searched for it: the recorded path itself when it is there, and otherwise
// the same session found by its file name in a working directory's folder, first of the recorded
// path's own sessions directory, then of the one paneSessions resolves, where the pane's harness
// keeps its sessions (Launcher.SessionsDir), resolved only when the recorded path is not there;
// nil is none. Oh My Pi keeps a session as
// `<sessions>/<folder named for the working directory>/<timestamp>_<session id>.jsonl` (Claude Code
// as `<projects>/<folder>/<session id>.jsonl`), so a session carried onto this host from where it
// was recorded — a pod, whose path names the image's home and profile — sits in a folder of the
// pane's own sessions directory, named for the pod's working directory or the pane's: each search
// looks one level below a sessions directory for a file of the recorded name, or, for Oh My Pi's
// name, of any timestamp with the recorded session id. Of several in one directory, the one written
// last is taken. found is "" when no directory holds the session; err is a failure to look, never
// the session's absence.
func sessionFileOf(recorded string, paneSessions func() (string, error)) (found string, searched []string, err error) {
	if _, err := os.Stat(recorded); err == nil {
		return recorded, nil, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	searched = []string{filepath.Dir(filepath.Dir(recorded))}
	if paneSessions != nil {
		pane, err := paneSessions()
		if err != nil {
			return "", nil, err
		}
		if pane != "" && pane != searched[0] {
			searched = append(searched, pane)
		}
	}
	name := filepath.Base(recorded)
	for _, sessions := range searched {
		if found, err = newestSession(sessions, name); err != nil || found != "" {
			return found, searched, err
		}
	}
	return "", searched, nil
}

// newestSession is the regular file written last among those one folder below sessions named name
// or, for Oh My Pi's `<timestamp>_<session id>.jsonl`, of any timestamp with name's session id; ""
// for none. A directory or name holding a glob character is searched nowhere: it would match other
// sessions.
func newestSession(sessions, name string) (string, error) {
	if strings.ContainsAny(sessions+name, `*?[\`) {
		return "", nil
	}
	patterns := []string{filepath.Join(sessions, "*", name)}
	if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
		if _, id, ok := strings.Cut(stem, "_"); ok && id != "" {
			patterns = append(patterns, filepath.Join(sessions, "*", "*_"+id+".jsonl"))
		}
	}
	var candidates []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			if !slices.Contains(candidates, match) {
				candidates = append(candidates, match)
			}
		}
	}
	newest, newestAt := "", time.Time{}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = candidate, info.ModTime()
		}
	}
	return newest, nil
}

// searchedIn is the sessions directories a search for a session found nothing in (sessionFileOf),
// as a refusal names them.
func searchedIn(searched []string) string {
	if len(searched) == 2 {
		return searched[0] + " or of the pane's sessions directory " + searched[1]
	}
	return strings.Join(searched, ", ")
}

// recordedElsewhere is whether, under runtime.tmux.take_over, the session recorded at path was
// recorded by the runtime this one takes over: a path under neither paneSessions, the pane's own
// sessions directory, nor the state directory, below which this runtime places every pane's home
// and harness directories. Such a session found nowhere is the takeover's even with no previous
// incarnation to say so — a claim the other runtime left suspended, or one whose takeover launch
// failed before this daemon restarted — and starts a fresh agent (Resume). Without the key nothing
// is, and a session missing from this host's own places is always the refusal.
func (r *Runtime) recordedElsewhere(path, paneSessions string) bool {
	return r.takeOver != "" && !within(path, paneSessions) && !within(path, r.stateDir)
}

// within is whether path is dir or lies below it; an empty dir holds nothing.
func within(path, dir string) bool {
	if dir == "" {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
