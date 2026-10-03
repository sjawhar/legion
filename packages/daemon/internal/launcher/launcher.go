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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// Config is the role-private launcher configuration. Token is read from the role-private
// projected Secret before Run is called. PrivateDir is the role container's own memory-backed
// directory, which no other container mounts: each generation's credentials live in a fresh
// `g<generation>` directory under it for exactly that generation's lifetime.
type Config struct {
	Connect, Token, Sandbox, Role, PodUID, PrivateDir string
	Stdout, Stderr                                    io.Writer
}

// reconnectDelay is the pause between daemon connections. A lost connection never stops the
// child: a daemon restart re-adopts the role process the launcher still owns.
const reconnectDelay = time.Second

// Run serves launcher commands until ctx ends. A stopped child is reported, never restarted by
// the launcher: the daemon owns generations and decides whether a new generation should start.
// Only ctx's end (the container's SIGTERM) stops the child the launcher owns.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	address, err := tcpAddress(cfg.Connect)
	if err != nil {
		return err
	}
	m := &manager{cfg: cfg, requests: map[string]request{}}
	launcherID := randomID()
	for {
		err := m.serve(ctx, address, launcherID)
		if ctx.Err() != nil {
			m.stopActive()
			return ctx.Err()
		}
		if cfg.Stderr != nil {
			fmt.Fprintf(cfg.Stderr, "launcher: daemon connection: %v; reconnecting\n", err)
		}
		select {
		case <-ctx.Done():
			m.stopActive()
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// serve is one authenticated daemon connection: hello, acknowledgement, the launcher's actual
// state, then start and stop commands until the connection ends.
func (m *manager) serve(ctx context.Context, address, launcherID string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", address, err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	writer := shimwire.NewWriter(conn)
	if err := writer.WriteFrame(shimwire.LauncherHello{
		Token: m.cfg.Token, Sandbox: m.cfg.Sandbox, Role: m.cfg.Role, PodUID: m.cfg.PodUID, LauncherID: launcherID,
	}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	reader := bufio.NewReaderSize(conn, shimwire.MaxHelloBytes+1)
	frame, err := readFrame(reader)
	if err != nil {
		return fmt.Errorf("hello acknowledgement: %w", err)
	}
	if _, ok := frame.(shimwire.LauncherHelloAck); !ok {
		return fmt.Errorf("hello acknowledgement: got %T, want launcher_hello_ack", frame)
	}
	m.mu.Lock()
	m.writer = writer
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.writer == writer {
			m.writer = nil
		}
		m.mu.Unlock()
	}()
	if err := m.state(); err != nil {
		return err
	}
	for {
		frame, err := readFrame(reader)
		if err != nil {
			return fmt.Errorf("read command: %w", err)
		}
		var answer shimwire.Frame
		switch command := frame.(type) {
		case shimwire.LauncherStart:
			answer = m.start(command)
		case shimwire.LauncherStop:
			answer = m.stop(command)
		default:
			return fmt.Errorf("command: got %T", frame)
		}
		if err := writer.WriteFrame(answer); err != nil {
			return fmt.Errorf("answer %T: %w", answer, err)
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
	case !filepath.IsAbs(c.PrivateDir):
		return fmt.Errorf("launcher: private directory %q is not absolute", c.PrivateDir)
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
	dir        string
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

// state reports the actual child to the current daemon connection, if one is up. A state change
// while disconnected is reported by the next connection's first state frame.
func (m *manager) state() error {
	m.mu.Lock()
	state := shimwire.LauncherState{LastExit: m.lastExit}
	if m.child != nil {
		state.Child = &shimwire.LauncherChild{Generation: m.child.generation, PID: m.child.pid}
	}
	writer := m.writer
	m.mu.Unlock()
	if writer == nil {
		return nil
	}
	if err := writer.WriteFrame(state); err != nil {
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
		result, matches := prior.result.(shimwire.LauncherStartResult)
		m.mu.Unlock()
		if !matches || string(prior.body) != string(body) {
			return shimwire.LauncherStartResult{ID: command.ID, Error: "launcher_start reused an id with a different payload"}
		}
		return result
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
	// A resume of a session the tree volume no longer holds is a launch failure, never a fresh
	// agent: the same rule workspace-init enforces for a pod's first start.
	if command.ResumeFile != "" {
		if _, err := os.Stat(command.ResumeFile); err != nil {
			result := shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("resume session file %s: %v", command.ResumeFile, err)}
			m.remember(command.ID, body, result)
			return result
		}
	}
	dir, err := m.writeFiles(command)
	if err != nil {
		result := shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
		m.remember(command.ID, body, result)
		return result
	}
	cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
	cmd.Env = mergeEnv(os.Environ(), command.Env)
	cmd.Stdout, cmd.Stderr = m.cfg.Stdout, m.cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		result := shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("start child: %v", err)}
		m.remember(command.ID, body, result)
		return result
	}
	active := &child{generation: command.Generation, pid: cmd.Process.Pid, dir: dir, done: make(chan struct{})}
	m.mu.Lock()
	m.child = active
	result := shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation}
	m.requests[command.ID] = request{body: body, result: result}
	m.mu.Unlock()
	go m.wait(cmd, active)
	_ = m.state()
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
	// The generation's credentials end with it.
	_ = os.RemoveAll(active.dir)
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
		result, matches := prior.result.(shimwire.LauncherStopResult)
		m.mu.Unlock()
		if !matches || string(prior.body) != string(body) {
			return shimwire.LauncherStopResult{ID: command.ID, Error: "launcher_stop reused an id with a different payload"}
		}
		return result
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

// generationDir is where generation's credentials live under the private directory.
func generationDir(private string, generation uint64) string {
	return filepath.Join(private, "g"+strconv.FormatUint(generation, 10))
}

// writeFiles materializes command's credentials in a fresh directory for its generation. No child
// runs when it is called, so everything already in the private directory — an earlier
// generation's credentials a crashed launcher never removed, or a directory, file or symlink the
// previous generation's agent planted toward the shared workspace — is removed without being
// followed; the directory is made owner-only and each file is created exclusively without
// following a link, so no credential is written anywhere but that directory. An error names the
// file, never its bytes.
func (m *manager) writeFiles(command shimwire.LauncherStart) (string, error) {
	entries, err := os.ReadDir(m.cfg.PrivateDir)
	if err != nil {
		return "", fmt.Errorf("read the private directory: %v", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(m.cfg.PrivateDir, entry.Name())); err != nil {
			return "", fmt.Errorf("clear %s from the private directory: %v", entry.Name(), err)
		}
	}
	dir := generationDir(m.cfg.PrivateDir, command.Generation)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("make credential directory for generation %d: %v", command.Generation, err)
	}
	for name, content := range command.Files {
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if err == nil {
			_, err = file.WriteString(content)
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write credential %s for generation %d: %v", name, command.Generation, err)
		}
	}
	return dir, nil
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
