// Package workerbin installs the directories at the head of a Legion agent's PATH, under a root
// directory: worker-bin, whose gh is the shim that routes the agent's gh through `legion gh`, and,
// where the agents run on a host, bin, whose legion launcher execs the legion that installed it: the
// daemon's, for a tmux daemon's panes, and the operator's, for the controller `legion controller
// start` runs. A pod's init container installs the shim alone, because a pod's PATH names the
// image's legion.
package workerbin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// DirName is the name of the gh interception directory under any root: `legion gh` and `legion
// credential` drop every PATH entry of that name before they look for the real gh.
const DirName = "worker-bin"

// Dir is root's worker-bin, the gh interception point.
func Dir(root string) string { return filepath.Join(root, DirName) }

// LauncherDir is root's bin, the directory of the legion launcher.
func LauncherDir(root string) string { return filepath.Join(root, "bin") }

// InstallGh atomically installs root's worker-bin/gh. The shim removes only its own leading
// directory from PATH before execing legion gh, so that child reaches the legion next on PATH and
// then the real gh.
func InstallGh(root string) error {
	bin := Dir(root)
	return installScript(bin, "gh", "#!/bin/sh\nPATH=${PATH#"+shellprefix.Literal(bin+string(filepath.ListSeparator))+"}\nexport PATH\nexec legion gh -- \"$@\"\n")
}

// Install installs the gh shim and bin's legion launcher, which execs legionExecutable (the shipped
// daemon's legionCliLauncherScript), so an agent under root never reaches another `legion` on the
// PATH it inherits. Its two callers pass their own executable: a tmux daemon's boot
// (internal/daemon) and `legion controller start` on the operator's machine (cmd/legion).
func Install(root, legionExecutable string) error {
	if !filepath.IsAbs(legionExecutable) {
		return fmt.Errorf("legion launcher target %q is not an absolute path", legionExecutable)
	}
	if err := InstallGh(root); err != nil {
		return err
	}
	return installScript(LauncherDir(root), "legion", "#!/bin/sh\nexec "+shellprefix.Literal(legionExecutable)+" \"$@\"\n")
}

// Path makes root's worker-bin and bin the first two entries of path, exactly once. A daemon
// started from an agent's shell can inherit its predecessor's directories; removing every
// occurrence avoids a recursive legion gh child while keeping every other entry verbatim.
func Path(path, root string) string {
	bin, launcher := Dir(root), LauncherDir(root)
	entries := make([]string, 0, len(filepath.SplitList(path))+2)
	entries = append(entries, bin, launcher)
	for _, entry := range filepath.SplitList(path) {
		if entry != "" && entry != bin && entry != launcher {
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
