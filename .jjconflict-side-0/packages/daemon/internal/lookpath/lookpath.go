// Package lookpath resolves an executable on a PATH string this process did not necessarily
// start with: a pod's or a daemon child's, not this binary's own. It is exec.LookPath's search
// alone, without exec.LookPath's refusal to run what it found (the daemon never execs the result
// itself in the same step that resolved it) and without its own-process PATH default, since every
// caller here already has the PATH it means to search.
package lookpath

import (
	"os"
	"path/filepath"
)

// On resolves name on searchPath, filepath.SplitList's elements in order: the first regular file
// with an execute bit wins. An empty element is skipped, as it is for exec.LookPath, and so is a
// relative one, as exec.LookPath refuses the result it would give. The config and tmux resolvers
// this package replaced accepted a relative element; that is the one change from their behaviour.
func On(searchPath, name string) (string, bool) {
	for _, dir := range filepath.SplitList(searchPath) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}
