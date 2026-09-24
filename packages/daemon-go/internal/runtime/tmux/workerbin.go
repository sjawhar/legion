package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

const (
	workerBinName = "worker-bin"
	legionBinName = "bin"
)

// InstallWorkerBin atomically installs the two directories at the head of every pane's PATH:
// worker-bin's gh interception point, and bin's `legion` launcher, which execs this daemon's own
// executable (the shipped daemon's legionCliLauncherScript) so a pane never reaches another
// `legion` on the operator's PATH. The gh shim removes only its own leading directory before
// execing legion gh, so that child reaches the launcher and then the real gh.
func InstallWorkerBin(stateDir, legionExecutable string) error {
	if !filepath.IsAbs(legionExecutable) {
		return fmt.Errorf("legion launcher target %q is not an absolute path", legionExecutable)
	}
	bin := workerBinDir(stateDir)
	if err := installScript(bin, "gh", "#!/bin/sh\nPATH=${PATH#"+shellprefix.Literal(bin+string(filepath.ListSeparator))+"}\nexport PATH\nexec legion gh -- \"$@\"\n"); err != nil {
		return err
	}
	return installScript(legionBinDir(stateDir), "legion", "#!/bin/sh\nexec "+shellprefix.Literal(legionExecutable)+" \"$@\"\n")
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

func workerBinDir(stateDir string) string { return filepath.Join(stateDir, workerBinName) }
func legionBinDir(stateDir string) string { return filepath.Join(stateDir, legionBinName) }

// workerPath makes this daemon's worker-bin and bin directories the first two entries exactly
// once. A daemon started from a worker pane can inherit its predecessor's directories; removing
// every occurrence avoids a recursive legion gh child while preserving every other entry verbatim.
func workerPath(path, stateDir string) string {
	bin, launcher := workerBinDir(stateDir), legionBinDir(stateDir)
	entries := make([]string, 0, len(filepath.SplitList(path))+2)
	entries = append(entries, bin, launcher)
	for _, entry := range filepath.SplitList(path) {
		if entry != "" && entry != bin && entry != launcher {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}
