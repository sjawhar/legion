//go:build linux

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// install writes the helper's systemd user unit for the binary at exe (symlinks resolved), the
// broker URL from --url or AGENT_SECRETS_URL and each of AGENT_SECRETS_HELPER_SOCK,
// AGENT_SECRETS_OPERATOR_FILE and AGENT_SECRETS_MACHINE_FILE set in its environment, so the unit's
// helper uses the socket install probes and the machine file the caller named, in three steps. First the preflight: a helper
// answering on its socket while the unit is not active was not started by it, and starting the
// unit would put a second helper on that socket, so install refuses, listing that helper's
// sessions, and writes and starts nothing, whether or not the unit file already matches. Then
// it writes the unit when its bytes differ, and runs `systemctl --user daemon-reload` and
// `enable --now` when it wrote it or the unit is not active. Last the restart: a helper that was
// answering before is restarted when restartWithSessions is set, whether or not the unit
// changed; otherwise only when the unit changed and `drain` finds it has no sessions (the
// helper then refuses new ones). A restart ends the sessions' grants, so with sessions it prints
// them and why it did not restart, and returns nil. A restart that fails after a drain sends
// `undrain`, so the helper takes sessions again, and returns the restart's error. A helper older
// than `drain` cannot refuse a register between the check and the restart, so without
// restartWithSessions it is never restarted: install prints its sessions and that it predates
// drain, and returns nil. An unchanged, active unit without restartWithSessions changes nothing.
func install(args []string, getenv func(string) string, exe string, stdout io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	url := fs.String("url", "", "the broker's base URL (default $AGENT_SECRETS_URL)")
	restartWithSessions := fs.Bool("restart-with-sessions", false, "restart the helper even when sessions are registered")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		*url = getenv("AGENT_SECRETS_URL")
	}
	*url = strings.TrimSuffix(*url, "/")
	if *url == "" {
		return errors.New("install needs the broker's URL: pass --url URL or set AGENT_SECRETS_URL")
	}
	const needsEscaping = " \t\r\n\"'\\%$;"
	if strings.ContainsAny(*url, needsEscaping) {
		return fmt.Errorf("broker URL %q holds characters a unit's Environment= line would need escaped", *url)
	}
	environment := "Environment=AGENT_SECRETS_URL=" + *url + "\n"
	for _, name := range []string{"AGENT_SECRETS_HELPER_SOCK", "AGENT_SECRETS_OPERATOR_FILE", "AGENT_SECRETS_MACHINE_FILE"} {
		value := getenv(name)
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, needsEscaping) {
			return fmt.Errorf("%s %q holds characters a unit's Environment= line would need escaped", name, value)
		}
		// The unit's helper runs in another working directory than install does.
		if !filepath.IsAbs(value) {
			return fmt.Errorf("%s %q is a relative path, which the unit's helper would resolve from another directory; set it to an absolute path", name, value)
		}
		environment += "Environment=" + name + "=" + value + "\n"
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve this binary's path: %w", err)
	}
	// ExecStart splits on whitespace and expands % specifiers and $ variables.
	if strings.ContainsAny(resolved, needsEscaping) {
		return fmt.Errorf("this binary's path %q holds characters ExecStart would split or expand; install it at a plain path", resolved)
	}
	unit := []byte(`[Unit]
Description=agent-secrets helper: pins agent sessions by pidfd and signs their secrets-broker proofs
[Service]
ExecStart=` + resolved + ` serve
` + environment + `# A SIGTERM from anything but systemd ends the helper with exit 0, so restart on any exit.
Restart=always
RestartSec=10
[Install]
WantedBy=default.target
`)

	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	running, err := helper.Call(cfg.Socket, helper.Request{Op: "sessions"}, 2*time.Second)
	wasRunning := err == nil
	active := unitActive()
	if wasRunning && !active {
		for _, s := range running.Sessions {
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", s.PID, s.RuntimeID, s.EnrollmentID, s.State)
		}
		return fmt.Errorf("an agent-secrets-helper not run by %s answers at %s (its sessions are listed above); stop it, then re-run install", helper.UnitName, cfg.Socket)
	}
	path := helper.UnitPath(getenv)
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed := !bytes.Equal(old, unit) || errors.Is(err, os.ErrNotExist)
	if changed {
		if err := writeFileAtomic(path, unit); err != nil {
			return err
		}
	}
	if changed || !active {
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		if err := systemctl("enable", "--now", helper.UnitName); err != nil {
			return err
		}
	}
	if !wasRunning || (!changed && !*restartWithSessions) {
		return nil
	}
	drained := false
	if !*restartWithSessions {
		var sessions []helper.SessionInfo
		sessions, drained, err = drainHelper(cfg.Socket)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", s.PID, s.RuntimeID, s.EnrollmentID, s.State)
		}
		if !drained {
			fmt.Fprintln(stdout, "not restarted: the running agent-secrets-helper predates drain, so a session could start between the check and the restart. Re-run with --restart-with-sessions at a quiet moment.")
			return nil
		}
		if n := len(sessions); n > 0 {
			fmt.Fprintf(stdout, "not restarted: %d sessions are registered; a restart ends their grants. Re-run with --restart-with-sessions.\n", n)
			return nil
		}
	}
	if err := systemctl("restart", helper.UnitName); err != nil {
		if drained {
			if resp, uerr := helper.Call(cfg.Socket, helper.Request{Op: "undrain"}, 2*time.Second); uerr != nil || !resp.OK {
				fmt.Fprintf(stdout, "the helper is still draining (refusing new sessions): undrain failed: %v %s\n", uerr, resp.Error)
			}
		}
		return err
	}
	return nil
}

// unitActive reports whether `systemctl --user is-active` names the helper's unit active.
func unitActive() bool {
	out, err := exec.Command("systemctl", "--user", "is-active", helper.UnitName).Output()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// drainHelper asks the helper at sock to drain: it answers its sessions and, with none, refuses
// every later register until it exits, so none can start between the answer and the restart.
// A helper older than the drain op answers it as an unknown op; drainHelper then answers that
// helper's `sessions` and drained false.
func drainHelper(sock string) (sessions []helper.SessionInfo, drained bool, err error) {
	resp, err := helper.Call(sock, helper.Request{Op: "drain"}, 2*time.Second)
	drained = true
	if err == nil && resp.Code == helper.CodeBadRequest && strings.HasPrefix(resp.Error, "unknown op") {
		drained = false
		resp, err = helper.Call(sock, helper.Request{Op: "sessions"}, 2*time.Second)
	}
	if err != nil {
		return nil, false, fmt.Errorf("agent-secrets-helper stopped answering at %s: %w", sock, err)
	}
	if !resp.OK {
		return nil, false, fmt.Errorf("agent-secrets-helper: %s: %s", resp.Code, resp.Error)
	}
	return resp.Sessions, drained, nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
