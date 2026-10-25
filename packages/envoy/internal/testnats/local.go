// This file is temporary. It exists because a Legion worker pod has no container runtime at all —
// no docker, podman or nerdctl, no /var/run/docker.sock, no DOCKER_HOST — so testcontainers
// panics there ("rootless Docker not found") and every Envoy Go test that asks for a NATS server
// fails before it runs. dispatch://INFRA-250 replaces the whole arrangement with an in-process
// nats-server behind testnats's existing seams and deletes this file, LocalServerEnv and
// scripts/e2e-local.sh's own local-server fallback together.

package testnats

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// LocalServerEnv names a nats-server binary the package's shared account server (URL) runs as a
// process of this machine instead of a container. CI has Docker and sets it nowhere, so a CI run
// is unchanged; a development pod has no Docker at all, so testcontainers cannot run there and
// every test that asks for an account would fail on the container start. It is the account
// server's variable alone: Start, StartRestartable and StartImageConfig still run containers,
// since a test that stops, restarts or reconfigures its own server reads the container's log and
// signals it through Docker.
//
// It is the same arrangement cmd/dispatch's multiprocess tests already use for their NATS
// (DISPATCH_TEST_NATS_URL), with the server's lifetime held here rather than by the caller.
const LocalServerEnv = "ENVOY_TEST_NATS_BIN"

// accountHost is the shared account server's host, and the two things handing out accounts asks of
// it: growing its account list by a reload, and ending it when the test binary ends.
type accountHost interface {
	// endpoint is the server's host:port for a client URL.
	endpoint() string
	// reload replaces the server's whole configuration with config and returns this reload's
	// outcome once the server has logged it: reloaded, or the server's own refusal.
	reload(t testing.TB, config string) string
	// terminate ends the server and frees what it holds.
	terminate() error
}

// startAccountHost starts the shared account server declaring accounts accounts, each under the
// one password secret: a local nats-server process when LocalServerEnv names one, and otherwise a
// container.
func startAccountHost(accounts int, secret string) (accountHost, error) {
	if binary := strings.TrimSpace(os.Getenv(LocalServerEnv)); binary != "" {
		return startLocalServer(binary, accountsConfig(accounts, secret))
	}
	return startContainerHost(accounts, secret)
}

// containerHost is the shared account server as a NATS test container.
type containerHost struct {
	ctr  *tcnats.NATSContainer
	host string
}

func (c containerHost) endpoint() string { return c.host }

func (c containerHost) reload(t testing.TB, config string) string {
	t.Helper()
	return reload(t, c.ctr, moduleConfigPath, config)
}

func (c containerHost) terminate() error { return testcontainers.TerminateContainer(c.ctr) }

// localServer is the shared account server as a nats-server process of this machine, with its
// configuration, JetStream store and log in a directory of its own.
type localServer struct {
	cmd  *exec.Cmd
	dir  string
	host string
}

// localListening is the line nats-server logs once it accepts client connections, carrying the
// port it chose: the server is started with -p -1, so the port is one nothing else holds at that
// moment, read from the server itself rather than from a pick this process would have to release
// first.
var localListening = regexp.MustCompile(`Listening for client connections on [^:\r\n]+:(\d+)`)

// startLocalServer runs binary with config, JetStream on, on a port it chooses, and returns it
// once it reports the port it listens on.
func startLocalServer(binary, config string) (accountHost, error) {
	dir, err := os.MkdirTemp("", "testnats-local-")
	if err != nil {
		return nil, fmt.Errorf("make the local NATS directory: %w", err)
	}
	server := &localServer{dir: dir}
	if err := server.writeConfig(config); err != nil {
		return nil, server.failed(err)
	}
	// The server listens on every interface, as the container backend's published port does, and
	// as internal/bus's remote-NATS tests need: they dial this server by a non-loopback address
	// of the machine to prove the guard on a NATS server that is not this machine's. What keeps
	// that reachable port from handing an account to anything that finds it is the password
	// accountsConfig writes, drawn once per test binary rather than being the account's own name.
	server.cmd = exec.Command(binary,
		"-c", server.configPath(), "-p", "-1", "-l", server.logPath())
	if err := server.cmd.Start(); err != nil {
		return nil, server.failed(fmt.Errorf("start %s: %w", binary, err))
	}
	port, err := server.awaitPort()
	if err != nil {
		return nil, server.failed(err)
	}
	server.host = "127.0.0.1:" + port
	return server, nil
}

func (s *localServer) configPath() string { return filepath.Join(s.dir, "nats.conf") }

func (s *localServer) logPath() string { return filepath.Join(s.dir, "nats.log") }

func (s *localServer) endpoint() string { return s.host }

// writeConfig writes config as the server's whole configuration, with JetStream and its store
// directory declared in the file rather than passed as -js and -sd: a reload reads the file
// alone, and the server refuses a reload whose file does not declare the storage directory it is
// running on ("config reload not supported for jetstream storage dir").
func (s *localServer) writeConfig(config string) error {
	whole := fmt.Sprintf("jetstream { store_dir: %q }\n%s", filepath.Join(s.dir, "store"), config)
	if err := os.WriteFile(s.configPath(), []byte(whole), 0o600); err != nil {
		return fmt.Errorf("write the local NATS configuration: %w", err)
	}
	return nil
}

// failed ends a half-started server and returns err, so a start that fails leaves no process and
// no directory behind.
func (s *localServer) failed(err error) error {
	if stop := s.terminate(); stop != nil {
		return fmt.Errorf("%w (and %w)", err, stop)
	}
	return err
}

// awaitPort waits, within connectTimeout, for the server to log the port it listens on.
func (s *localServer) awaitPort() (string, error) {
	deadline := time.Now().Add(connectTimeout)
	for time.Now().Before(deadline) {
		if match := localListening.FindStringSubmatch(s.log()); match != nil {
			return match[1], nil
		}
		time.Sleep(retryInterval)
	}
	return "", fmt.Errorf("local NATS did not report its port within %s; its log says: %s", connectTimeout, s.log())
}

// log is everything the server has logged so far, and "" before it has written its log file.
func (s *localServer) log() string {
	text, err := os.ReadFile(s.logPath())
	if err != nil {
		return ""
	}
	return string(text)
}

// reload writes config and signals the server to reload it, as reload does for a container: the
// server keeps its connections, and the outcome returned is this reload's, not an earlier one's.
func (s *localServer) reload(t testing.TB, config string) string {
	t.Helper()
	earlier := len(reloadOutcome.FindAllString(s.log(), -1))
	if err := s.writeConfig(config); err != nil {
		t.Fatalf("write the new configuration: %v", err)
	}
	if err := s.cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal NATS to reload: %v", err)
	}
	deadline := time.Now().Add(connectTimeout)
	for time.Now().Before(deadline) {
		if outcomes := reloadOutcome.FindAllString(s.log(), -1); len(outcomes) > earlier {
			return outcomes[earlier]
		}
		time.Sleep(retryInterval)
	}
	t.Fatalf("NATS did not report reloading its configuration within %s", connectTimeout)
	return ""
}

// terminate kills the server and removes its directory, which holds its JetStream store.
func (s *localServer) terminate() error {
	if s.cmd != nil && s.cmd.Process != nil {
		// os.ErrProcessDone is what Kill reports for a server that exited on its own - a crash,
		// or a test that stopped it already - and is nothing to fail the cleanup over.
		if err := s.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop the local NATS: %w", err)
		}
		_ = s.cmd.Wait()
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("remove the local NATS directory: %w", err)
	}
	return nil
}
