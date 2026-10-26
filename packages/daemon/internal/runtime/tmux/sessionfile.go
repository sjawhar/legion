package tmux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// sessionFileOf is the file a resume continues the claim's recorded session from: the recorded
// path itself when it is there, and otherwise the same session found by its file name in another
// working directory's folder of the same sessions directory. Oh My Pi keeps a session as
// `<sessions>/<folder named for the working directory>/<timestamp>_<session id>.jsonl` (Claude Code
// as `<projects>/<folder>/<session id>.jsonl`), so a session recorded in a pod and carried onto
// this host keeps its folder, named for the pod's working directory, while the pane works in
// another: the search looks one level below the recorded path's sessions directory for a file of
// the recorded name, or, for Oh My Pi's name, of any timestamp with the recorded session id. Of
// several, the one written last is taken. None is an error wrapping runtime.ErrSessionFileMissing.
func sessionFileOf(recorded, harness string) (string, error) {
	if _, err := os.Stat(recorded); err == nil {
		return recorded, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	sessions, name := filepath.Dir(filepath.Dir(recorded)), filepath.Base(recorded)
	var found []string
	// A name or directory holding a glob character is searched for nowhere: it would match others.
	if !strings.ContainsAny(sessions+name, `*?[\`) {
		patterns := []string{filepath.Join(sessions, "*", name)}
		if stem, ok := strings.CutSuffix(name, ".jsonl"); ok {
			if _, id, ok := strings.Cut(stem, "_"); ok && id != "" {
				patterns = append(patterns, filepath.Join(sessions, "*", "*_"+id+".jsonl"))
			}
		}
		for _, pattern := range patterns {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				return "", err
			}
			for _, match := range matches {
				if !slices.Contains(found, match) {
					found = append(found, match)
				}
			}
		}
	}
	newest, newestAt := "", time.Time{}
	for _, candidate := range found {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(newestAt) {
			newest, newestAt = candidate, info.ModTime()
		}
	}
	if newest == "" {
		return "", fmt.Errorf("%w: no %s session at %s, nor of that session in another folder of %s", runtime.ErrSessionFileMissing, harness, recorded, sessions)
	}
	return newest, nil
}
