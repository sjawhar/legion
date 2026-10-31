package main

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// syncBuffer is a bytes.Buffer the launcher writes from its role goroutines while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An issue pod's one container runs `legion launcher --roles-dir <dir> --role <r>…`, the command
// the daemon's pod builder renders: one connection per role, each with the token in that role's
// own directory, the pod's uid from POD_UID as the single-role launcher always took it, and a
// log whose lines each name their role.
func TestTheLauncherCommandServesEveryRoleOfARolesDirectory(t *testing.T) {
	rolesDir := t.TempDir()
	for _, role := range []string{"tester", "reviewer"} {
		for _, name := range []string{"launcher", "private"} {
			if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(rolesDir, role, "launcher", "LAUNCHER_TOKEN"), []byte("token-"+role+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POD_UID", "pod-7")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- run(ctx, []string{
			"legion", "launcher", "--connect", "tcp://" + listener.Addr().String(), "--sandbox", "legion-acme-1",
			"--roles-dir", rolesDir, "--role", "tester", "--role", "reviewer", "--stop-grace", "2s",
		}, &stdout, &stderr)
	}()

	hellos := map[string]shimwire.LauncherHello{}
	for range 2 {
		conn, err := listener.Accept()
		if err != nil {
			t.Fatalf("accept: %v; the launcher's log: %s", err, stderr.String())
		}
		t.Cleanup(func() { _ = conn.Close() })
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		hello, ok := frame.(shimwire.LauncherHello)
		if !ok {
			t.Fatalf("first frame = %#v, want a launcher hello", frame)
		}
		hellos[hello.Role] = hello
		if err := shimwire.NewWriter(conn).WriteFrame(shimwire.LauncherHelloAck{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"tester", "reviewer"} {
		hello := hellos[role]
		if hello.Token != "token-"+role || hello.Sandbox != "legion-acme-1" || hello.PodUID != "pod-7" {
			t.Fatalf("role %s hello = %#v, want its own token, the sandbox and POD_UID's pod", role, hello)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "tester: launcher: connected") || !strings.Contains(stderr.String(), "reviewer: launcher: connected") {
		if time.Now().After(deadline) {
			t.Fatalf("the launcher logged %q, want each role's acknowledged hello behind its role's name", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit %d at the container's stop; stderr: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the launcher did not leave at the container's stop")
	}
}

// The two forms do not mix: a roles directory holds every role's own token and private
// directory, and the controller pod's single form names one role. Each mixture is a usage error
// before anything is dialled.
func TestTheLauncherCommandRefusesAMixOfItsTwoForms(t *testing.T) {
	token := filepath.Join(t.TempDir(), "LAUNCHER_TOKEN")
	if err := os.WriteFile(token, []byte("token"), 0o400); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a roles directory and a token file", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--token-file", token}, "no --token-file or --private-dir"},
		{"a roles directory and a private directory", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--private-dir", t.TempDir()}, "no --token-file or --private-dir"},
		{"two roles without a roles directory", []string{"--token-file", token, "--role", "tester", "--role", "reviewer", "--private-dir", t.TempDir()}, "several roles need --roles-dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A launcher that took the mixture would dial the unreachable daemon until its
			// context ends; the bound makes that a failure of this test, not a hung run.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			args := append([]string{"legion", "launcher", "--connect", "tcp://127.0.0.1:1", "--sandbox", "s", "--pod-uid", "p"}, tc.args...)
			if code := run(ctx, args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 2 naming %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// `legion launcher role-init` is the launcher's own, hidden from its usage, and refuses to run
// anywhere but as the init of a role's new namespaces: run by hand it would otherwise mount a
// role's private view over the caller's own. Every refusal, its usage's included, is a failure
// before the shim started, so it exits launcher.RoleInitSetupFailedExit, the one status the
// launcher reports as a setup failure, never the 2 a panic exits with.
func TestRoleInitIsHiddenAndRefusesToRunOutsideARolesNamespaces(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"legion", "launcher", "--help"}, &stdout, &stderr); code != 0 || strings.Contains(stderr.String(), "role-init") {
		t.Fatalf("launcher --help: exit %d, %q; want its usage without the hidden role-init", code, stderr.String())
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"legion", "launcher", "role-init", "--roles-dir", "/r", "--role", "a", "--status-fd", "3"}, &stdout, &stderr); code != launcher.RoleInitSetupFailedExit {
		t.Fatalf("role-init with no shim argv: exit %d, %q; want its usage and exit %d", code, stderr.String(), launcher.RoleInitSetupFailedExit)
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"legion", "launcher", "role-init", "--no-such-flag"}, &stdout, &stderr); code != launcher.RoleInitSetupFailedExit {
		t.Fatalf("role-init with a flag it does not know: exit %d, %q; want %d", code, stderr.String(), launcher.RoleInitSetupFailedExit)
	}
	stderr.Reset()
	code := run(context.Background(), []string{"legion", "launcher", "role-init", "--roles-dir", t.TempDir(), "--role", "a", "--generation", "1", "--status-fd", "3", "--", "/bin/true"}, &stdout, &stderr)
	if code != launcher.RoleInitSetupFailedExit || !strings.Contains(stderr.String(), "role-init: not the init of a PID namespace of its own") {
		t.Fatalf("role-init outside a role's namespaces: exit %d, %q; want a refusal with exit %d", code, stderr.String(), launcher.RoleInitSetupFailedExit)
	}
}
