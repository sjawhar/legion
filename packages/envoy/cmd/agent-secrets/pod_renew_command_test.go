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
	"context"
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
	// A directory a shell would split without quoting, and whose apostrophe a single-quoted
	// word has to close, escape and reopen (shellWord): a command that pasted the path raw, or
	// quoted it naively, names another directory and finds no key.
	out := filepath.Join(t.TempDir(), "key d'ir")

	// The environment every command here runs in: this process's, with every AGENT_SECRETS_*
	// and XDG_* variable dropped before the ones below are set. Dropping rather than overriding
	// the few that are named is the point. `agent-secrets` picks its identity from a key dir, a
	// persisted sign-in, this machine's own identity or a helper socket, in that order
	// (selectSigner), and each reads an environment variable of its own —
	// AGENT_SECRETS_SIGNIN_DIR, or $XDG_STATE_HOME/agent-secrets/signins where it is unset
	// (brokerclient.SignIns), and AGENT_SECRETS_MACHINE_IDENTITY with the token file, audience
	// and pool settings beside it. A devbox that has run `agent-secrets login` carries those,
	// so a command here that inherited them would find a person's identity, renew that person's
	// unrelated enrollment against this stub broker and retry its 404 forever: the control would
	// hang rather than exit 2, and the test would prove nothing about the key directory.
	//
	// The three commands share one HOME, one runtime dir, one state dir and one sign-in dir
	// because they are one pod: the enrolment writes its key into --out, so all four stay empty,
	// which is what makes the key directory reachable only through the printed command.
	home, runtimeDir, stateHome, signInDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	environment := func() []string {
		var env []string
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(name, "AGENT_SECRETS_") || strings.HasPrefix(name, "XDG_") {
				continue
			}
			env = append(env, entry)
		}
		return append(env,
			"HOME="+home,
			"XDG_RUNTIME_DIR="+runtimeDir,
			"XDG_STATE_HOME="+stateHome,
			"AGENT_SECRETS_SIGNIN_DIR="+signInDir,
			"AGENT_SECRETS_URL="+broker.URL,
			"AGENT_SECRETS_KEY_DIR=",
			"AGENT_SECRETS_HELPER_SOCK=",
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
	// The shape of the line, never the path in it: the directory is shell-quoted, so one
	// holding a space or an apostrophe is correctly printed unlike itself. That the variable
	// names the directory this run wrote to is proved below by running the line, from a
	// directory whose name a shell would split.
	if !strings.HasPrefix(command, "AGENT_SECRETS_KEY_DIR=") {
		t.Fatalf("the printed command %q does not begin with the key directory", command)
	}

	// The control: the same command with the key directory taken off it, which is what was
	// printed before. It finds no session identity and exits at once, so the variable is what
	// makes the printed command work rather than anything else in the environment. It is run
	// under a deadline because an identity leaking in from the environment would make it a
	// renewal loop instead, and a loop that retried forever would hang the package rather than
	// name what went wrong.
	bareCtx, cancelBare := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelBare()
	bare := exec.CommandContext(bareCtx, "/bin/sh", "-c", "agent-secrets renew --pod-token-file "+shellWord(tokenFile))
	bare.Env = environment()
	bare.Cancel = func() error { return syscall.Kill(-bare.Process.Pid, syscall.SIGKILL) }
	bare.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output, err := bare.CombinedOutput()
	if bareCtx.Err() != nil {
		t.Fatalf("the command without the key directory did not exit in 60s; it found an identity it should not have. Output %q", output)
	}
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
	// This stderr is read while the command still runs — the test watches for the renewal and
	// then reads what it said — and os/exec copies the pipe into it on a goroutine of its own
	// until Wait, so it is the locked writer renew_test.go already keeps for exactly this and
	// never a bare strings.Builder. A renewal the broker refuses 5xx makes the renewer print a
	// retry line on every attempt, which is when the unlocked read raced the copy.
	var renewErr syncWriter
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
