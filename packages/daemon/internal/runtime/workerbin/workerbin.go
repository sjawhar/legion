// Package workerbin installs the directory at the head of a host-run Legion agent's PATH, under a
// root directory: bin, whose legion launcher execs the legion that installed it — the daemon's,
// for a tmux daemon's panes, and the operator's, for the controller `legion controller start`
// runs — so an agent under root never reaches another `legion` on the PATH it inherits. Nothing
// here intercepts gh: an agent's gh, git and jj are whatever its PATH resolves (a pane's the
// daemon's PATH, a pod's the image's), and its GitHub credential is the gh files under its
// GH_CONFIG_DIR (internal/ghconfig). A pod installs nothing: its PATH names the image's legion.
package workerbin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// LauncherDir is root's bin, the directory of the legion launcher.
func LauncherDir(root string) string { return filepath.Join(root, "bin") }

// Install installs bin's legion launcher, which execs legionExecutable (the shipped daemon's
// legionCliLauncherScript). Its two callers pass their own executable: a tmux daemon's boot
// (internal/daemon) and `legion controller start` on the operator's machine (cmd/legion).
func Install(root, legionExecutable string) error {
	if !filepath.IsAbs(legionExecutable) {
		return fmt.Errorf("legion launcher target %q is not an absolute path", legionExecutable)
	}
	return installScript(LauncherDir(root), "legion", "#!/bin/sh\nexec "+shellprefix.Literal(legionExecutable)+" \"$@\"\n")
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
