package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Every refusal the shipped command makes before it dials or spawns (worker-shim.ts:630-676,
// 590-620), each naming the flag or path at fault: the daemon's pane has nothing to show but
// this line, and a shim that dialled first would register a pane that can never start OMP.
func TestWorkerShimRefusesBeforeDialOrSpawn(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	connect := "unix://" + socket

	token := filepath.Join(dir, "token")
	blank := filepath.Join(dir, "blank")
	for path, body := range map[string]string{token: "boot\n", blank: "  \n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	providers := filepath.Join(dir, "providers")
	if err := os.Mkdir(providers, 0o700); err != nil {
		t.Fatal(err)
	}
	shadow := filepath.Join(providers, "LEGION_SHIM_TEST_SHADOWED")
	if err := os.WriteFile(shadow, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_SHIM_TEST_SHADOWED", "")

	marker := filepath.Join(dir, "spawned")
	omp := []string{"--", "sh", "-c", "touch " + marker}
	for _, tc := range []struct {
		name string
		args []string
		code int
		says []string
	}{
		{"no --connect", append([]string{"--boot-token-file", token}, omp...), 1, []string{"--connect is required"}},
		{"a malformed --connect", append([]string{"--connect", "tcp://127.0.0.1", "--boot-token-file", token}, omp...), 1, []string{"--connect", `"tcp://127.0.0.1"`}},
		{"no --boot-token-file", append([]string{"--connect", connect}, omp...), 1, []string{"--boot-token-file"}},
		{"an unreadable token file", append([]string{"--connect", connect, "--boot-token-file", filepath.Join(dir, "absent")}, omp...), 1, []string{"--boot-token-file " + filepath.Join(dir, "absent") + " is unreadable"}},
		{"a blank token file", append([]string{"--connect", connect, "--boot-token-file", blank}, omp...), 1, []string{"--boot-token-file " + blank + " is blank"}},
		{"a providers key the shim's environment already has", append([]string{"--connect", connect, "--boot-token-file", token, "--provider-env-dir", providers}, omp...), 1, []string{"LEGION_SHIM_TEST_SHADOWED", shadow}},
		{"an unreadable providers directory", append([]string{"--connect", connect, "--boot-token-file", token, "--provider-env-dir", filepath.Join(dir, "absent")}, omp...), 1, []string{"--provider-env-dir " + filepath.Join(dir, "absent") + " is unreadable"}},
		{"no wrapped command", []string{"--connect", connect, "--boot-token-file", token, "--"}, 1, []string{"no wrapped command"}},
		{"--socket mode, which is not ported", append([]string{"--socket", socket}, omp...), 2, []string{"-socket"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), append([]string{"legion", "worker-shim"}, tc.args...), &stdout, &stderr)
			if code != tc.code {
				t.Fatalf("exit %d, want %d; stderr: %s", code, tc.code, stderr.String())
			}
			for _, want := range tc.says {
				if !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr %q does not name %q", stderr.String(), want)
				}
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the wrapped command was spawned before the refusal")
			}
			_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
			if conn, err := ln.Accept(); err == nil {
				_ = conn.Close()
				t.Fatal("the shim dialled the daemon before the refusal")
			} else if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("accept: %v", err)
			}
		})
	}
}

// The command end to end: the hello carries the file's token, the providers directory reaches
// the child's environment only, and the shim's exit status is the child's — which is what the
// pane, and the runtime watching it, see.
func TestWorkerShimBridgesTheChildAndExitsWithItsStatus(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("boot-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	providers := filepath.Join(dir, "providers")
	if err := os.Mkdir(providers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(providers, "LEGION_SHIM_TEST_PROVIDED"), []byte(" provided\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(dir, "seen")

	daemon := make(chan error, 1)
	go func() {
		daemon <- func() error {
			conn, err := ln.Accept()
			if err != nil {
				return err
			}
			defer conn.Close()
			line, err := shimwire.NewReader(conn).ReadLine()
			if err != nil {
				return err
			}
			if hello, err := shimwire.Decode(line); err != nil || hello != (shimwire.Hello{BootToken: "boot-token"}) {
				return errors.New("the first frame was " + string(line))
			}
			return shimwire.NewWriter(conn).WriteFrame(shimwire.HelloAck{})
		}()
	}()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"legion", "worker-shim",
		"--connect", "unix://" + socket, "--boot-token-file", token, "--provider-env-dir", providers,
		"--", "sh", "-c", `printf %s "$LEGION_SHIM_TEST_PROVIDED" > "$0"; exit 7`, seen}, &stdout, &stderr)
	if code != 7 {
		t.Fatalf("exit %d, want the child's 7; stderr: %s; stdout: %s", code, stderr.String(), stdout.String())
	}
	select {
	case err := <-daemon:
		if err != nil {
			t.Fatalf("the daemon side: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shim never completed the handshake")
	}
	if got, err := os.ReadFile(seen); err != nil || string(got) != "provided" {
		t.Fatalf("the child saw LEGION_SHIM_TEST_PROVIDED=%q (%v), want the providers file's trimmed contents", got, err)
	}
	if _, ok := os.LookupEnv("LEGION_SHIM_TEST_PROVIDED"); ok {
		t.Fatal("the providers key leaked into the shim's own environment")
	}
}

// With --pod-safety (the Sandbox runtime passes it; a pane never does) the shim starts the agent on
// the pod's baseline: the overlay written under LEGION_STATE_DIR and named first in
// PI_CONFIG_FILES, ahead of the operator's, and the baseline variables where the pod leaves them
// unset. Without it the agent starts on the environment it always had. With --pod-safety and no
// state directory the shim refuses naming it, before anything is dialled or spawned.
func TestWorkerShimStartsAPodsAgentOnTheBaselineAndAPanesAsBefore(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("boot-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	t.Setenv("PI_CONFIG_FILES", "/etc/operator.yml")
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("LEGION_STATE_DIR", "")
	marker := filepath.Join(dir, "spawned")
	check := `touch "$0"; [ "$PI_CONFIG_FILES" = "$1" ] && [ "${OTEL_SDK_DISABLED-unset}" = "$2" ] && exit 7; echo "PI_CONFIG_FILES=$PI_CONFIG_FILES OTEL_SDK_DISABLED=${OTEL_SDK_DISABLED-unset}" >&2; exit 8`
	shim := func(podSafety bool, overlays, otel string) []string {
		args := []string{"legion", "worker-shim", "--connect", "unix://" + socket, "--boot-token-file", token}
		if podSafety {
			args = append(args, "--pod-safety")
		}
		return append(args, "--", "sh", "-c", check, marker, overlays, otel)
	}
	acknowledge := func() chan error {
		daemon := make(chan error, 1)
		go func() {
			daemon <- func() error {
				conn, err := ln.Accept()
				if err != nil {
					return err
				}
				defer conn.Close()
				if _, err := shimwire.NewReader(conn).ReadLine(); err != nil {
					return err
				}
				if err := shimwire.NewWriter(conn).WriteFrame(shimwire.HelloAck{}); err != nil {
					return err
				}
				// Held open until the shim closes it: a stream the daemon drops is one the shim
				// dials again, which would outlive the agent's exit.
				_, err = io.Copy(io.Discard, conn)
				return err
			}()
		}()
		return daemon
	}

	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), shim(true, "", ""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "LEGION_STATE_DIR") {
		t.Fatalf("--pod-safety with no state directory: exit %d, stderr %q; want exit 1 naming LEGION_STATE_DIR", code, stderr.String())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent was spawned with no baseline to start it on")
	}
	_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(20 * time.Millisecond))
	if conn, err := ln.Accept(); err == nil {
		_ = conn.Close()
		t.Fatal("the shim dialled the daemon with no baseline to start the agent on")
	}
	_ = ln.(*net.UnixListener).SetDeadline(time.Time{})

	t.Setenv("LEGION_STATE_DIR", state)
	for name, tc := range map[string]struct {
		podSafety      bool
		overlays, otel string
	}{
		"a pod":  {true, filepath.Join(state, "podsafety-overlay.yml") + ":/etc/operator.yml", "true"},
		"a pane": {false, "/etc/operator.yml", ""},
	} {
		daemon := acknowledge()
		stdout.Reset()
		stderr.Reset()
		if code := run(context.Background(), shim(tc.podSafety, tc.overlays, tc.otel), &stdout, &stderr); code != 7 {
			t.Fatalf("%s: exit %d, want 7: the agent starts with PI_CONFIG_FILES %q and OTEL_SDK_DISABLED %q; stderr: %s", name, code, tc.overlays, tc.otel, stderr.String())
		}
		if err := <-daemon; err != nil {
			t.Fatalf("%s: the daemon side: %v", name, err)
		}
	}
}
