package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const workerBinName = "worker-bin"

// InstallWorkerBin atomically installs the gh interception point every worker pane receives. The
// script removes its own leading directory before execing legion gh, so legion gh's child reaches
// the real gh binary rather than recursing into this shim.
func InstallWorkerBin(stateDir string) error {
	bin := workerBinDir(stateDir)
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return fmt.Errorf("create worker bin %s: %w", bin, err)
	}
	if err := os.Chmod(bin, 0o700); err != nil {
		return fmt.Errorf("chmod worker bin %s: %w", bin, err)
	}

	shim := filepath.Join(bin, "gh")
	temporary, err := os.CreateTemp(bin, ".gh-")
	if err != nil {
		return fmt.Errorf("create worker gh shim: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	contents := "#!/bin/sh\nPATH=${PATH#" + shellLiteral(bin+string(filepath.ListSeparator)) + "}\nexport PATH\nexec legion gh -- \"$@\"\n"
	if _, err := temporary.WriteString(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write worker gh shim: %w", err)
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod worker gh shim: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close worker gh shim: %w", err)
	}
	if err := os.Rename(temporaryPath, shim); err != nil {
		return fmt.Errorf("install worker gh shim: %w", err)
	}
	return nil
}

func workerBinDir(stateDir string) string { return filepath.Join(stateDir, workerBinName) }

// workerPath makes this daemon's own worker-bin directory the first entry exactly once. A daemon
// started from a worker pane can inherit its predecessor's shim; removing every occurrence avoids
// a recursive legion gh child while preserving every other path entry verbatim.
func workerPath(path, stateDir string) string {
	bin := workerBinDir(stateDir)
	entries := make([]string, 0, len(filepath.SplitList(path))+1)
	entries = append(entries, bin)
	for _, entry := range filepath.SplitList(path) {
		if entry != "" && entry != bin {
			entries = append(entries, entry)
		}
	}
	return strings.Join(entries, string(filepath.ListSeparator))
}

func shellLiteral(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
