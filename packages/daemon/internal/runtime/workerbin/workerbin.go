// Package workerbin installs the directory at the head of a host-run Legion agent's PATH, under a
// root directory: bin, whose legion launcher execs the legion that installed it — the daemon's,
// for a tmux daemon's panes, and the operator's, for the controller `legion controller start`
// runs — so an agent under root never reaches another `legion` on the PATH it inherits, and whose
// gh execs that legion's `legion gh`, which runs the real gh (LEGION_GH_PATH) with the gh directory
// of the owner the command names (internal/ghconfig's bundle). An agent's git and jj are whatever
// its PATH resolves. A pod installs nothing: the image carries the same two scripts in
// /opt/legion/bin, which its PATH names first.
package workerbin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// LauncherDir is root's bin, the directory of the legion launcher and the gh wrapper.
func LauncherDir(root string) string { return filepath.Join(root, "bin") }

// Install installs bin's legion launcher, which execs legionExecutable, and its gh, which execs
// legionExecutable's `legion gh`, the path absolute and single-quoted (shellprefix.Literal) in
// both. Its two callers pass their own executable: a tmux daemon's boot (internal/daemon) and
// `legion controller start` on the operator's machine (cmd/legion).
func Install(root, legionExecutable string) error {
	if !filepath.IsAbs(legionExecutable) {
		return fmt.Errorf("legion launcher target %q is not an absolute path", legionExecutable)
	}
	legion := shellprefix.Literal(legionExecutable)
	if err := installScript(LauncherDir(root), "legion", "#!/bin/sh\nexec "+legion+" \"$@\"\n"); err != nil {
		return err
	}
	return installScript(LauncherDir(root), "gh", "#!/bin/sh\nexec "+legion+" gh \"$@\"\n")
}

// Path makes root's bin the first entry of path, exactly once. A daemon started from an agent's
// shell can inherit its predecessor's directory; removing every occurrence keeps PATH from growing
// while keeping every other entry verbatim.
func Path(path, root string) string {
	launcher := LauncherDir(root)
	entries := make([]string, 0, len(filepath.SplitList(path))+1)
	entries = append(entries, launcher)
	for _, entry := range filepath.SplitList(path) {
		if entry != "" && entry != launcher {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

// installScript writes one 0700 script into a 0700 directory, replacing any earlier one atomically.
func installScript(dir, name, contents string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return fmt.Errorf("create %s script: %w", name, err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.WriteString(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write %s script: %w", name, err)
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod %s script: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s script: %w", name, err)
	}
	if err := os.Rename(temporaryPath, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("install %s script: %w", name, err)
	}
	return nil
}
