// Package launcher runs one role container's long-lived process supervisor. It connects to the
// daemon independently from the child worker shim, reports actual child state after every
// reconnect, and owns exactly one worker-shim process group at a time.
package launcher

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Config is the role-private launcher configuration. Token is read from the role-private
// projected Secret before Run is called. BootTokenFile is a role-private memory-backed path; no
// other container mounts it.
type Config struct {
	Connect, Token, Sandbox, Role, PodUID, BootTokenFile string
	Stdout, Stderr                                      io.Writer
}

// Run serves launcher commands until ctx ends. A stopped child is reported, never restarted by
// the launcher: the daemon owns generations and decides whether a new generation should start.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	address, err := tcpAddress(cfg.Connect)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("launcher dial %s: %w", cfg.Connect, err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()

	m := manager{cfg: cfg, writer: shimwire.NewWriter(conn), requests: map[string]request{}}
	if err := m.writer.WriteFrame(shimwire.LauncherHello{
		Token: cfg.Token, Sandbox: cfg.Sandbox, Role: cfg.Role, PodUID: cfg.PodUID, LauncherID: randomID(),
	}); err != nil {
		return fmt.Errorf("launcher hello: %w", err)
	}
	reader := bufio.NewReaderSize(conn, shimwire.MaxHelloBytes+1)
	frame, err := readFrame(reader)
	if err != nil {
		return fmt.Errorf("launcher hello acknowledgement: %w", err)
	}
	if _, ok := frame.(shimwire.LauncherHelloAck); !ok {
		return fmt.Errorf("launcher hello acknowledgement: got %T, want launcher_hello_ack", frame)
	}
	if err := m.state(); err != nil {
		return err
	}
	for {
		frame, err := readFrame(reader)
		if err != nil {
			if ctx.Err() != nil {
				m.stopActive()
				return ctx.Err()
			}
			m.stopActive()
			return fmt.Errorf("launcher read command: %w", err)
		}
		switch command := frame.(type) {
		case shimwire.LauncherStart:
			result := m.start(command)
			if err := m.writer.WriteFrame(result); err != nil {
				m.stopActive()
				return fmt.Errorf("launcher start result: %w", err)
			}
		case shimwire.LauncherStop:
			result := m.stop(command)
			if err := m.writer.WriteFrame(result); err != nil {
				m.stopActive()
				return fmt.Errorf("launcher stop result: %w", err)
			}
		default:
			m.stopActive()
			return fmt.Errorf("launcher command: got %T", frame)
		}
	}
}

func (c Config) validate() error {
	switch {
	case c.Connect == "":
		return errors.New("launcher: no connect address")
	case c.Token == "":
		return errors.New("launcher: no token")
	case c.Sandbox == "":
		return errors.New("launcher: no sandbox")
	case c.Role == "":
		return errors.New("launcher: no role")
	case c.PodUID == "":
		return errors.New("launcher: no pod UID")
	case !filepath.IsAbs(c.BootTokenFile):
		return fmt.Errorf("launcher: boot token file %q is not absolute", c.BootTokenFile)
	}
	return nil
}

func tcpAddress(addr string) (string, error) {
	address, ok := strings.CutPrefix(addr, "tcp://")
	if !ok || address == "" {
		return "", fmt.Errorf("launcher: connect %q is not tcp://host:port", addr)
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return "", fmt.Errorf("launcher: connect %q is not tcp://host:port: %w", addr, err)
	}
	return address, nil
}

func readFrame(reader *bufio.Reader) (shimwire.Frame, error) {
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	frame, err := shimwire.Decode(line)
	if err != nil {
		return nil, err
	}
	return frame, nil
}

type request struct {
	body   []byte
	result shimwire.Frame
}

type child struct {
	generation uint64
	pid        int
	done       chan struct{}
}

type manager struct {
	cfg      Config
	writer   *shimwire.Writer
	mu       sync.Mutex
	child    *child
	lastExit *shimwire.LauncherExit
	requests map[string]request
}

func (m *manager) state() error {
	m.mu.Lock()
	state := shimwire.LauncherState{LastExit: m.lastExit}
	if m.child != nil {
		state.Child = &shimwire.LauncherChild{Generation: m.child.generation, PID: m.child.pid}
	}
	m.mu.Unlock()
	if err := m.writer.WriteFrame(state); err != nil {
		return fmt.Errorf("launcher state: %w", err)
	}
	return nil
}

func (m *manager) start(command shimwire.LauncherStart) shimwire.LauncherStartResult {
	if err := command.Validate(); err != nil {
		return shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
	}
	body, _ := json.Marshal(command)
	m.mu.Lock()
	if prior, ok := m.requests[command.ID]; ok {
		m.mu.Unlock()
		if string(prior.body) != string(body) {
			return shimwire.LauncherStartResult{ID: command.ID, Error: "launcher_start reused an id with a different payload"}
		}
		return prior.result.(shimwire.LauncherStartResult)
	}
	if current := m.child; current != nil {
		m.mu.Unlock()
		return shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("generation %d is still running", current.generation)}
	}
	if m.lastExit != nil && command.Generation <= m.lastExit.Generation {
		m.mu.Unlock()
		return shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("generation %d already exited", command.Generation)}
	}
	m.mu.Unlock()
	if err := writeBootToken(m.cfg.BootTokenFile, command.BootToken); err != nil {
		result := shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
		m.remember(command.ID, body, result)
		return result
	}
	cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
	cmd.Env = mergeEnv(os.Environ(), command.Env)
	cmd.Stdout, cmd.Stderr = m.cfg.Stdout, m.cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		result := shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("start child: %v", err)}
		m.remember(command.ID, body, result)
		return result
	}
	active := &child{generation: command.Generation, pid: cmd.Process.Pid, done: make(chan struct{})}
	m.mu.Lock()
	m.child = active
	result := shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation}
	m.requests[command.ID] = request{body: body, result: result}
	m.mu.Unlock()
	go m.wait(cmd, active)
	return result
}

func (m *manager) remember(id string, body []byte, result shimwire.LauncherStartResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[id] = request{body: body, result: result}
}

func (m *manager) wait(cmd *exec.Cmd, active *child) {
	err := cmd.Wait()
	exit := exitOf(err, active.generation)
	m.mu.Lock()
	if m.child == active {
		m.child = nil
		m.lastExit = &exit
	}
	close(active.done)
	m.mu.Unlock()
	_ = m.state()
}

func (m *manager) stop(command shimwire.LauncherStop) shimwire.LauncherStopResult {
	if err := command.Validate(); err != nil {
		return shimwire.LauncherStopResult{ID: command.ID, Error: err.Error()}
	}
	body, _ := json.Marshal(command)
	m.mu.Lock()
	if prior, ok := m.requests[command.ID]; ok {
		m.mu.Unlock()
		if string(prior.body) != string(body) {
			return shimwire.LauncherStopResult{ID: command.ID, Error: "launcher_stop reused an id with a different payload"}
		}
		return prior.result.(shimwire.LauncherStopResult)
	}
	active := m.child
	m.mu.Unlock()
	if active == nil {
		result := shimwire.LauncherStopResult{ID: command.ID, OK: true}
		m.rememberStop(command.ID, body, result)
		return result
	}
	if active.generation != command.Generation {
		result := shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("generation %d is running, not %d", active.generation, command.Generation)}
		m.rememberStop(command.ID, body, result)
		return result
	}
	if err := syscall.Kill(-active.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		result := shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("signal child process group: %v", err)}
		m.rememberStop(command.ID, body, result)
		return result
	}
	select {
	case <-active.done:
	case <-time.After(time.Duration(command.GraceMs) * time.Millisecond):
		if err := syscall.Kill(-active.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			result := shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("kill child process group: %v", err)}
			m.rememberStop(command.ID, body, result)
			return result
		}
		<-active.done
	}
	result := shimwire.LauncherStopResult{ID: command.ID, OK: true}
	m.rememberStop(command.ID, body, result)
	return result
}

func (m *manager) rememberStop(id string, body []byte, result shimwire.LauncherStopResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[id] = request{body: body, result: result}
}

func (m *manager) stopActive() {
	m.mu.Lock()
	active := m.child
	m.mu.Unlock()
	if active == nil {
		return
	}
	_ = syscall.Kill(-active.pid, syscall.SIGTERM)
	select {
	case <-active.done:
	case <-time.After(time.Second):
		_ = syscall.Kill(-active.pid, syscall.SIGKILL)
		<-active.done
	}
}

func writeBootToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("launcher boot token directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("launcher boot token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("launcher publish boot token: %w", err)
	}
	return nil
}

func mergeEnv(base, updates []string) []string {
	values := map[string]string{}
	order := make([]string, 0, len(base)+len(updates))
	add := func(entry string) {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, entry := range base {
		add(entry)
	}
	for _, entry := range updates {
		add(entry)
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+values[name])
	}
	return out
}

func exitOf(err error, generation uint64) shimwire.LauncherExit {
	exit := shimwire.LauncherExit{Generation: generation}
	var process *exec.ExitError
	if errors.As(err, &process) {
		if status, ok := process.Sys().(syscall.WaitStatus); ok {
			exit.Code = status.ExitStatus()
			if status.Signaled() {
				exit.Signal = status.Signal().String()
			}
		}
	}
	return exit
}

func randomID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}
