package gcp_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// shellCommand is the command that runs the golden's shell scripts and bootcmd entries in these
// tests, with args after it: /bin/sh, the shell cloud-init and systemd run them with, or the words
// of ENVOY_TEST_SHELL, as "bash" or "busybox sh", to run the same rows under another shell.
// Container-Optimized OS ships both dash and bash (COS 133's cos-package-info.json lists
// app-shells/dash 0.5.13.5 and app-shells/bash 5.3_p3-r3), so the rows run under both.
func shellCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	shell := strings.Fields(os.Getenv("ENVOY_TEST_SHELL"))
	if len(shell) == 0 {
		shell = []string{"/bin/sh"}
	}
	return exec.Command(shell[0], append(shell[1:], args...)...)
}
