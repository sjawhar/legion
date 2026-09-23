package tmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallWorkerBinInstallsPrivateExecutableGhShim(t *testing.T) {
	stateDir := t.TempDir()
	if err := InstallWorkerBin(stateDir); err != nil {
		t.Fatalf("InstallWorkerBin: %v", err)
	}
	bin := filepath.Join(stateDir, "worker-bin")
	if info, err := os.Stat(bin); err != nil {
		t.Fatalf("worker-bin: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("worker-bin mode = %o, want 0700", info.Mode().Perm())
	}
	shim := filepath.Join(bin, "gh")
	contents, err := os.ReadFile(shim)
	if err != nil {
		t.Fatalf("gh shim: %v", err)
	}
	if info, err := os.Stat(shim); err != nil {
		t.Fatalf("gh shim stat: %v", err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("gh shim mode = %o, want 0700", info.Mode().Perm())
	}
	want := "#!/bin/sh\nPATH=${PATH#'" + bin + ":'}\nexport PATH\nexec legion gh -- \"$@\"\n"
	if string(contents) != want {
		t.Fatalf("gh shim = %q, want %q", contents, want)
	}
}

func TestPaneEnvironmentPutsWorkerBinFirstExactlyOnce(t *testing.T) {
	stateDir := "/var/lib/legion"
	workerBin := filepath.Join(stateDir, "worker-bin")
	env := PaneEnvironment([]string{
		"PATH=" + workerBin + ":/usr/local/bin:" + workerBin + ":/usr/bin",
	}, stateDir)
	if got, want := env["PATH"], workerBin+":/usr/local/bin:/usr/bin"; got != want {
		t.Fatalf("PATH = %q, want %q", got, want)
	}
	if strings.Count(env["PATH"], workerBin) != 1 {
		t.Fatalf("PATH = %q carries worker-bin more than once", env["PATH"])
	}
}
