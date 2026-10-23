// packages/envoy/cmd/agent-secrets-helper/main_test.go
//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
)

// registerEnv names the helper socket a child of this test binary registers with as a session
// root (registerAndWait), in place of running the tests.
const registerEnv = "AGENT_SECRETS_HELPER_TEST_REGISTER"

func TestMain(m *testing.M) {
	if sock := os.Getenv(registerEnv); sock != "" {
		registerAndWait(sock)
		return
	}
	os.Exit(m.Run())
}

// registerAndWait registers this process with the helper at sock, prints "registered", and lives
// until its stdin closes, so the helper keeps it as a session for as long as the test needs.
func registerAndWait(sock string) {
	if resp, err := helper.Call(sock, helper.Request{Op: "register"}, 5*time.Second); err != nil || !resp.OK {
		fmt.Fprintf(os.Stderr, "register: %+v %v\n", resp, err)
		os.Exit(1)
	}
	fmt.Println("registered")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigDerivesDefaultsAndServeRequiresURL(t *testing.T) {
	cfg, err := loadConfig(env(map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/7"}))
	if err != nil {
		t.Fatalf("sessions must work without AGENT_SECRETS_URL: %v", err)
	}
	if err := serve(cfg); err == nil || !strings.Contains(err.Error(), "AGENT_SECRETS_URL") {
		t.Fatalf("serve must refuse without AGENT_SECRETS_URL, got %v", err)
	}
	cfg, err = loadConfig(env(map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/7", "AGENT_SECRETS_URL": "https://secrets.internal.example"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Socket != "/run/user/7/agent-secrets/helper.sock" || cfg.StatePath != "/run/user/7/agent-secrets/sessions.json" ||
		cfg.OperatorFile != "/home/u/.config/agent-secrets/operator" || cfg.MachineFile != "/home/u/.config/agent-secrets/machine.json" {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg, _ = loadConfig(env(map[string]string{"HOME": "/home/u", "AGENT_SECRETS_URL": "https://s", "AGENT_SECRETS_HELPER_SOCK": "/tmp/x/h.sock", "AGENT_SECRETS_MACHINE_FILE": "/tmp/x/m.json"}))
	if cfg.StatePath != "/tmp/x/sessions.json" {
		t.Fatalf("state sits beside the socket: %q", cfg.StatePath)
	}
	if cfg.MachineFile != "/tmp/x/m.json" {
		t.Fatalf("AGENT_SECRETS_MACHINE_FILE must name the machine file: %q", cfg.MachineFile)
	}
}

// TestServeRefusesAMachineFileThatDoesNotParse: a machine file that is there but is not a machine
// login stops serve before it listens, naming the file, rather than serving with no login.
func TestServeRefusesAMachineFileThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	machineFile := filepath.Join(dir, "machine.json")
	if err := os.WriteFile(machineFile, []byte("not a login"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{URL: "https://s", Socket: dir + "/h.sock", OperatorFile: dir + "/none", MachineFile: machineFile, StatePath: dir + "/s.json"}
	if err := serve(cfg); err == nil || !strings.Contains(err.Error(), machineFile) {
		t.Fatalf("serve on an unreadable machine file: %v; want an error naming %s", err, machineFile)
	}
	if _, err := os.Stat(cfg.Socket); err == nil {
		t.Fatal("serve listened despite the unreadable machine file")
	}
}

// TestServeStartsWithoutCredentialAndAnswersSessions pins the brief's replacement for the old
// startup refusal: the unit always serves now, full stop — no TokenFile, and a missing
// OperatorFile does not stop it from listening and answering `sessions` either.
func TestServeStartsWithoutCredentialAndAnswersSessions(t *testing.T) {
	dir := t.TempDir()
	cfg := config{URL: "https://s", Socket: dir + "/h.sock", OperatorFile: dir + "/none", MachineFile: dir + "/machine.json", StatePath: dir + "/s.json"}
	done := make(chan error, 1)
	go func() { done <- serve(cfg) }()
	t.Cleanup(func() {
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(cfg.Socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := helper.Call(cfg.Socket, helper.Request{Op: "sessions"}, 2*time.Second)
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if !resp.OK {
		t.Fatalf("sessions must succeed even with no machine credential yet: %+v", resp)
	}
}

// fakeBroker answers the broker routes a logged-in helper calls: a machine login, as an OAuth
// device grant, that issues on the first token poll with a week's expiry, enrollments, and
// revokes.
func fakeBroker(t *testing.T) *httptest.Server {
	t.Helper()
	reply := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	var enrollments atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/device_authorization", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]any{"device_code": "device-1", "user_code": "KQ7M-X4PZ", "verification_uri": "https://s", "expires_in": 900, "interval": 1})
	})
	mux.HandleFunc("POST /v1/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]any{"access_token": "access-1", "token_type": "DPoP", "expires_in": 900, "refresh_token": "refresh-1",
			"credential_id": "lcred-1", "credential_expires_at": time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("POST /v1/enrollments", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusCreated, map[string]string{"enrollment_id": fmt.Sprintf("enr-%d", enrollments.Add(1)), "lease_expires_at": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339Nano)})
	})
	mux.HandleFunc("DELETE /v1/enrollments/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// registerChild starts a child of this test binary that registers with the helper at sock as a
// session root of its own, and keeps it alive until the test ends.
func registerChild(t *testing.T, sock string) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), registerEnv+"="+sock)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "registered\n" {
		t.Fatalf("the registering child printed %q (%v); want registered", line, err)
	}
}

// TestServeNamesTheSignalItStopsOn: a SIGTERM or SIGINT from anything but systemd (a test rig's
// `pkill -f "agent-secrets-helper serve"` matches the unit's helper too) stops the helper as
// systemd's own stop does, with exit 0, since a non-zero exit would leave every stop failed. The
// unit's journal then says only that it ended, so the helper's last line names the signal, how
// many sessions it had and that it held a launcher credential.
func TestServeNamesTheSignalItStopsOn(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agent-secrets-helper")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	for _, tc := range []struct {
		sig  syscall.Signal
		name string
	}{
		{syscall.SIGTERM, "terminated"},
		{syscall.SIGINT, "interrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stderr, err := os.Create(filepath.Join(dir, "stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			logged := func() string {
				b, err := os.ReadFile(stderr.Name())
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			operator := filepath.Join(dir, "operator")
			if err := os.WriteFile(operator, []byte("alice@example.com\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			sock := filepath.Join(dir, "run", "helper.sock")
			cmd := exec.Command(binary, "serve")
			cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "AGENT_SECRETS_URL=" + fakeBroker(t).URL,
				"AGENT_SECRETS_HELPER_SOCK=" + sock, "AGENT_SECRETS_OPERATOR_FILE=" + operator}
			cmd.Stderr = stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			exited := make(chan error, 1)
			go func() { exited <- cmd.Wait() }()
			stopped := false
			t.Cleanup(func() {
				if !stopped {
					_ = cmd.Process.Kill()
					<-exited
				}
			})
			waitFor := func(what string, cond func() bool) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for !cond() {
					if time.Now().After(deadline) {
						t.Fatalf("%s within 10 s; stderr:\n%s", what, logged())
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			waitFor("the helper answering on its socket", func() bool {
				resp, err := helper.Call(sock, helper.Request{Op: "sessions"}, time.Second)
				return err == nil && resp.OK
			})
			registerChild(t, sock)
			registerChild(t, sock)
			if resp, err := helper.Call(sock, helper.Request{Op: "login"}, 5*time.Second); err != nil || !resp.OK {
				t.Fatalf("login: %+v %v", resp, err)
			}
			waitFor("a launcher credential", func() bool {
				resp, err := helper.Call(sock, helper.Request{Op: "login-status"}, time.Second)
				return err == nil && resp.CredentialHeld
			})

			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-exited:
				stopped = true
				if err != nil {
					t.Fatalf("serve after %s: %v; want exit 0, as a requested stop. stderr:\n%s", tc.sig, err, logged())
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("serve did not stop on %s; stderr:\n%s", tc.sig, logged())
			}
			want := `level=WARN msg="agent-secrets-helper stopping on a signal" signal=` + tc.name + ` sessions=2 launcher_credential=true`
			if !strings.Contains(logged(), want) {
				t.Fatalf("stderr after %s:\n%s\nwant a line carrying %s", tc.sig, logged(), want)
			}
		})
	}
}

// TestClaimSocketRefusesALiveHelperAndClearsAStaleSocket: serve never takes over a socket another
// helper answers on (the error names it, and the socket still answers), and removes one nothing
// answers on, as a helper that died leaves.
func TestClaimSocketRefusesALiveHelperAndClearsAStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimSocket(sock); err == nil || !strings.Contains(err.Error(), sock) {
		t.Fatalf("claimSocket on a live socket: %v; want an error naming %s", err, sock)
	}
	if conn, err := net.Dial("unix", sock); err != nil {
		t.Fatalf("the live socket stopped answering: %v", err)
	} else {
		conn.Close()
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("the stale socket is not there to clear: %v", err)
	}
	if err := claimSocket(sock); err != nil {
		t.Fatalf("claimSocket on a stale socket: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("the stale socket is still there: %v", err)
	}
}
