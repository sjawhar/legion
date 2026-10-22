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

const unitName = "agent-secrets-helper.service"

// install writes the helper's systemd user unit for the binary at exe (symlinks resolved) and
// the broker URL from --url or AGENT_SECRETS_URL. When the unit's bytes are unchanged it does
// nothing more. Otherwise it writes the unit, runs `systemctl --user daemon-reload` and
// `enable --now`, and restarts a running helper (one whose socket answers `sessions`) only when
// it lists no sessions or restartWithSessions is set; a restart ends the sessions' grants, so
// with sessions it prints them and why it did not restart, and returns nil.
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
	if strings.ContainsAny(*url, " \t\r\n\"'\\%$;") {
		return fmt.Errorf("broker URL %q holds characters a unit's Environment= line would need escaped", *url)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve this binary's path: %w", err)
	}
	// ExecStart splits on whitespace and expands % specifiers and $ variables.
	if strings.ContainsAny(resolved, " \t\r\n\"'\\%$;") {
		return fmt.Errorf("this binary's path %q holds characters ExecStart would split or expand; install it at a plain path", resolved)
	}
	unit := []byte(`[Unit]
Description=agent-secrets helper: pins agent sessions by pidfd and signs their secrets-broker proofs
[Service]
ExecStart=` + resolved + ` serve
Environment=AGENT_SECRETS_URL=` + *url + `
# A SIGTERM from anything but systemd ends the helper with exit 0, so restart on any exit.
Restart=always
RestartSec=10
[Install]
WantedBy=default.target
`)

	configHome := getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(getenv("HOME"), ".config")
	}
	path := filepath.Join(configHome, "systemd", "user", unitName)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, unit) {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeFileAtomic(path, unit); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", unitName); err != nil {
		return err
	}

	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	resp, err := helper.Call(cfg.Socket, helper.Request{Op: "sessions"}, 2*time.Second)
	if err != nil || !resp.OK {
		// Not running: enable --now started it on the new unit.
		return nil
	}
	if n := len(resp.Sessions); n > 0 && !*restartWithSessions {
		for _, s := range resp.Sessions {
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", s.PID, s.RuntimeID, s.EnrollmentID, s.State)
		}
		fmt.Fprintf(stdout, "not restarted: %d sessions are registered; a restart ends their grants. Re-run with --restart-with-sessions.\n", n)
		return nil
	}
	return systemctl("restart", unitName)
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
