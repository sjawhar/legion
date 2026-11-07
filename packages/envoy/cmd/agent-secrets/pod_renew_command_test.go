//go:build unix

// packages/envoy/cmd/agent-secrets/pod_renew_command_test.go
//
// The renewal command `enroll --pod` prints, run exactly as printed. A pod enrolled into a
// directory named by --out alone holds its key nowhere AGENT_SECRETS_KEY_DIR points, and `renew`
// chooses its identity from that variable or from the $XDG_RUNTIME_DIR default and never from
// --out, so a command naming only the token file exited 2 with no session identity. This drives
// the line through a real shell and the built binary, from a directory whose name needs quoting,
// so both the variable and the quoting are proved by the renewal reaching the broker; the same
// command stripped of the variable is the control, and still finds nothing.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// printedRenewCommand is the command the enrolment named, taken out of its stderr line.
var printedRenewCommand = regexp.MustCompile(`keep this lease alive with: (.*)`)

// TestThePrintedRenewalCommandRunsFromAnOutDirectory enrols a pod with --out and no
// AGENT_SECRETS_KEY_DIR in the environment, then runs the printed command through /bin/sh with
// the variable still empty. The renewal must reach the broker, which it can only do by finding
// key.pem and enrollment in the directory the printed command names.
func TestThePrintedRenewalCommandRunsFromAnOutDirectory(t *testing.T) {
	binary := buildAgentSecrets(t)
	token := fakePodToken(t, testPodAccount, testPodUID)
	tokenFile := writePodToken(t, token)
	broker, calls := servePodBroker(t, "enrollment-of-the-pod")
	// A directory a shell would split without quoting, so a command that pasted the path raw
	// would name another directory and find no key.
	out := filepath.Join(t.TempDir(), "key dir")

	// The environment every command here runs in: the key directory is named by nothing but the
	// command itself, and the $XDG_RUNTIME_DIR default holds no key either.
	environment := func() []string {
		return append(os.Environ(), "AGENT_SECRETS_URL="+broker.URL, "AGENT_SECRETS_KEY_DIR=",
			"AGENT_SECRETS_HELPER_SOCK=", "XDG_RUNTIME_DIR="+t.TempDir(), "HOME="+t.TempDir(),
			// PATH's first entry is the built binary's own directory, so `agent-secrets` in a
			// printed command is this tree's.
			"PATH="+filepath.Dir(binary)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	enrolled := exec.Command(binary, "enroll", "--pod", "--service", testPodService, "--pod-token-file", tokenFile, "--out", out)
	enrolled.Env = environment()
	var enrolErr strings.Builder
	enrolled.Stderr = &enrolErr
	if err := enrolled.Run(); err != nil {
		t.Fatalf("enroll --pod --out %q: %v, stderr %q", out, err, enrolErr.String())
	}
	found := printedRenewCommand.FindStringSubmatch(enrolErr.String())
	if found == nil {
		t.Fatalf("stderr %q names no renewal command", enrolErr.String())
	}
	command := strings.TrimSpace(found[1])
	if !strings.Contains(command, "AGENT_SECRETS_KEY_DIR='"+out+"'") {
		t.Fatalf("the printed command %q does not carry the key directory %q, shell-quoted", command, out)
	}

	// The control: the same command with the key directory taken off it, which is what was
	// printed before. It finds no session identity and exits at once, so the variable is what
	// makes the printed command work rather than anything else in the environment.
	bare := exec.Command("/bin/sh", "-c", "agent-secrets renew --pod-token-file "+shellWord(tokenFile))
	bare.Env = environment()
	output, err := bare.CombinedOutput()
	if err == nil {
		t.Fatalf("the command without the key directory succeeded, output %q; want it refused", output)
	}
	if code := bare.ProcessState.ExitCode(); code != exitUsageError || !strings.Contains(string(output), "no session identity") {
		t.Fatalf("the command without the key directory = exit %d, output %q; want %d naming no session identity", code, output, exitUsageError)
	}

	// The printed command itself. `renew` runs until it is interrupted, so it is started in a
	// process group of its own and signalled there: the shell may fork rather than exec it.
	renew := exec.Command("/bin/sh", "-c", command)
	renew.Env = environment()
	renew.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var renewErr strings.Builder
	renew.Stderr = &renewErr
	if err := renew.Start(); err != nil {
		t.Fatalf("run the printed command: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-renew.Process.Pid, syscall.SIGKILL)
		_ = renew.Wait()
	}()

	renewed := func() bool {
		for _, call := range calls() {
			if call.path == "/v1/enrollments/enrollment-of-the-pod/renew" {
				return true
			}
		}
		return false
	}
	for deadline := time.Now().Add(30 * time.Second); !renewed(); {
		if time.Now().After(deadline) {
			t.Fatalf("the printed command sent no renewal in 30s; stderr %q", renewErr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := renewErr.String(); strings.Contains(got, "no session identity") {
		t.Fatalf("the printed command found no session identity: %q", got)
	}
}
