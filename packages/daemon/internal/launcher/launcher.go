// Package launcher runs a launcher container's long-lived process supervisor: one per role it
// serves, each of which connects to the daemon independently from its child worker shim, reports
// actual child state after every reconnect, and owns exactly one generation at a time.
package launcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// WorkspaceRecreatedVariable is how a role launcher tells the generation it starts whether that
// generation resumes a session in a workspace recreated since the session was last written: "true"
// or "false", set on every generation, so no value from the container's environment reaches the
// child. The Legion plugin tells the agent so in a message it saves to the session at its start,
// ahead of the next turn (pi-legion, src/workspace-recreated.ts).
const WorkspaceRecreatedVariable = "LEGION_WORKSPACE_RECREATED"

// Config is the launcher's configuration. The controller's pod names one role: Token, read from
// its role-private projected Secret before Run is called, Role, and PrivateDir, that container's
// own memory-backed directory, which no other container mounts. An issue pod's one container
// names RolesDir and every resident role instead: the launcher reads each role's token from
// <RolesDir>/<role>/launcher/LAUNCHER_TOKEN, writes that role's generations under
// <RolesDir>/<role>/private, and starts each of them in namespaces of its own (role-init), so no
// role reaches another's credentials, processes or files. Either way a generation's credentials
// live in a fresh `g<generation>` directory for exactly that generation's lifetime.
type Config struct {
	Connect, Token, Sandbox, Role, PodUID, PrivateDir string
	RolesDir                                          string
	Roles                                             []string
	Stdout, Stderr                                    io.Writer
	// StopGrace is the container shutdown and natural-exit descendant cleanup budget. Every role
	// of an issue pod's container is given it whole, since they stop together.
	StopGrace time.Duration
}

// reconnectDelay is the pause between daemon connections. A lost connection never stops the
// child: a daemon restart re-adopts the role process the launcher still owns.
const reconnectDelay = time.Second

// Run serves launcher commands until ctx ends. A stopped child is reported, never restarted by
// the launcher: the daemon owns generations and decides whether a new generation should start.
// Only ctx's end (the container's SIGTERM) stops the children the launcher owns, and in an issue
// pod's container it stops every role's at once, each with the whole stop grace.
func Run(ctx context.Context, cfg Config) error {
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 30 * time.Second
	}
	if cfg.StopGrace < 0 {
		return errors.New("launcher: stop grace must be positive")
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	address, err := tcpAddress(cfg.Connect)
	if err != nil {
		return err
	}
	managers, err := newManagers(cfg)
	if err != nil {
		return err
	}
	if cfg.RolesDir == "" {
		m := managers[0]
		// The controller's pod runs one role, whose processes are this PID namespace's: PID 1
		// adopts the role's orphans and reaps them. An issue pod's roles each have a namespace
		// of their own, where their own init does that, and this process has no child but those
		// inits, which its managers wait on.
		if os.Getpid() == 1 {
			exits := make(chan os.Signal, 1)
			signal.Notify(exits, syscall.SIGCHLD)
			defer signal.Stop(exits)
			go m.reapOrphans(ctx, exits)
		}
		return m.serveUntil(ctx, address)
	}
	var running sync.WaitGroup
	for _, m := range managers {
		running.Add(1)
		go func() {
			defer running.Done()
			_ = m.serveUntil(ctx, address)
		}()
	}
	running.Wait()
	return ctx.Err()
}

// newManagers is one manager per role the launcher serves, each with its own token, private
// directory, launcher id and, in an issue pod's container, its own prefix in the container's log.
func newManagers(cfg Config) ([]*manager, error) {
	if cfg.RolesDir == "" {
		return []*manager{{
			cfg: cfg, role: cfg.Role, token: cfg.Token, private: cfg.PrivateDir, launcherID: randomID(),
			stdout: cfg.Stdout, stderr: cfg.Stderr, requests: map[string]request{},
		}}, nil
	}
	log := &roleLog{stdout: cfg.Stdout, stderr: cfg.Stderr}
	managers := make([]*manager, 0, len(cfg.Roles))
	for _, role := range cfg.Roles {
		file := filepath.Join(cfg.RolesDir, role, RoleLauncherDir, LauncherTokenFile)
		token, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("launcher: role %s launcher token: %v", role, err)
		}
		if strings.TrimSpace(string(token)) == "" {
			return nil, fmt.Errorf("launcher: role %s launcher token %s is empty", role, file)
		}
		// The launcher's own lines for the role are written through a writer of their own, and
		// each generation's child through writers of its own (command), so no launcher line is
		// glued onto a line the child has not finished, and no generation's onto another's.
		_, own := log.writers(role)
		managers = append(managers, &manager{
			cfg: cfg, role: role, token: strings.TrimSpace(string(token)), namespaced: true,
			private: filepath.Join(cfg.RolesDir, role, RolePrivateDir), launcherID: randomID(),
			log: log, stderr: own, requests: map[string]request{},
		})
	}
	return managers, nil
}

// serveUntil serves one role's daemon connections until ctx ends, reconnecting between them, and
// stops that role's child when it does.
func (m *manager) serveUntil(ctx context.Context, address string) error {
	for {
		err := m.serve(ctx, address)
		if ctx.Err() != nil {
			m.stopActive()
			return ctx.Err()
		}
		m.logf("launcher: daemon connection: %v; reconnecting", err)
		select {
		case <-ctx.Done():
			m.stopActive()
			return ctx.Err()
		case <-time.After(reconnectDelay):
		}
	}
}

// logf writes one line of this role's log, behind its own prefix in an issue pod's container.
func (m *manager) logf(format string, args ...any) {
	if m.stderr == nil {
		return
	}
	fmt.Fprintf(m.stderr, format+"\n", args...)
}

// serve is one authenticated daemon connection: hello, acknowledgement, the launcher's actual
// state, then start and stop commands until the connection ends. The acknowledgement is logged as
// one line, so a launcher the daemon accepted reads apart from one still dialing.
func (m *manager) serve(ctx context.Context, address string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", address, err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	writer := shimwire.NewWriter(conn)
	if err := writer.WriteFrame(shimwire.LauncherHello{
		Token: m.token, Sandbox: m.cfg.Sandbox, Role: m.role, PodUID: m.cfg.PodUID, LauncherID: m.launcherID,
	}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	reader := shimwire.NewReader(conn)
	frame, err := readFrame(reader)
	if err != nil {
		return fmt.Errorf("hello acknowledgement: %w", err)
	}
	if _, ok := frame.(shimwire.LauncherHelloAck); !ok {
		return fmt.Errorf("hello acknowledgement: got %T, want launcher_hello_ack", frame)
	}
	m.logf("launcher: connected to the daemon at %s as launcher %s (sandbox %s, role %s, pod %s)",
		address, m.launcherID, m.cfg.Sandbox, m.role, m.cfg.PodUID)
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

// validate refuses a configuration that is neither of the launcher's two forms: the controller
// pod's one role with its own token and private directory, or an issue pod's roles directory
// holding every resident role's.
func (c Config) validate() error {
	switch {
	case c.Connect == "":
		return errors.New("launcher: no connect address")
	case c.Sandbox == "":
		return errors.New("launcher: no sandbox")
	case c.PodUID == "":
		return errors.New("launcher: no pod UID")
	}
	if c.RolesDir == "" {
		switch {
		case c.Token == "":
			return errors.New("launcher: no token")
		case c.Role == "":
			return errors.New("launcher: no role")
		case len(c.Roles) != 0:
			return errors.New("launcher: roles without a roles directory")
		case !filepath.IsAbs(c.PrivateDir):
			return fmt.Errorf("launcher: private directory %q is not absolute", c.PrivateDir)
		}
		return nil
	}
	switch {
	case c.Token != "" || c.Role != "" || c.PrivateDir != "":
		return errors.New("launcher: a roles directory holds every role's own token and private directory")
	case len(c.Roles) == 0:
		return errors.New("launcher: no roles")
	case !filepath.IsAbs(c.RolesDir):
		return fmt.Errorf("launcher: roles directory %q is not absolute", c.RolesDir)
	}
	seen := map[string]bool{}
	for _, role := range c.Roles {
		switch {
		case role == "":
			return errors.New("launcher: an empty role")
		case seen[role]:
			return fmt.Errorf("launcher: role %s twice", role)
		}
		seen[role] = true
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

func readFrame(reader *shimwire.Reader) (shimwire.Frame, error) {
	line, err := reader.ReadLine()
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

// child is the generation this role is running: in the controller's pod the shim itself, in an
// issue pod the role-init that is the shim's own namespaces' init, with the pipe it reports the
// shim's wait status on.
type child struct {
	generation uint64
	pid        int
	dir        string
	status     *os.File
	// output is the generation's own prefixed stdout and stderr in an issue pod's container,
	// whose unfinished last lines wait flushes once the generation's process is gone.
	output []io.Writer
	done   chan struct{}
}

// manager serves one role: its own daemon connection, its own launcher id, its own token and
// private directory, its own child, and, in an issue pod's container, its own prefix in the log
// and a namespace of its own for every generation it starts. stdout and stderr are where the
// controller's one role writes; in an issue pod's container stderr is the launcher's own lines
// for the role, and log gives each generation writers of its own.
type manager struct {
	cfg        Config
	role       string
	token      string
	private    string
	namespaced bool
	launcherID string
	stdout     io.Writer
	stderr     io.Writer
	log        *roleLog
	writer     *shimwire.Writer
	mu         sync.Mutex
	child      *child
	lastExit   *shimwire.LauncherExit
	requests   map[string]request
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
	env := mergeEnv(os.Environ(), command.Env)
	recreated := false
	if command.ResumeFile != "" {
		// An issue pod's role names its session under its own home, which the container holds
		// under the home mounts; the controller's pod mounts its home's volumes in place.
		onDisk := command.ResumeFile
		if m.namespaced {
			onDisk = containerPath(onDisk)
		}
		var err error
		if recreated, err = resumable(env, command.ResumeFile, onDisk, m.stderr); err != nil {
			result := shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
			m.remember(command.ID, body, result)
			return result
		}
	}
	env = mergeEnv(env, []string{WorkspaceRecreatedVariable + "=" + strconv.FormatBool(recreated)})
	dir, err := m.writeFiles(command)
	if err != nil {
		result := shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
		m.remember(command.ID, body, result)
		return result
	}
	process, err := m.command(command, env)
	if err != nil {
		_ = os.RemoveAll(dir)
		result := shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
		m.remember(command.ID, body, result)
		return result
	}
	// The orphan reaper must see this PID as the direct child before it can observe its exit.
	m.mu.Lock()
	if err := process.start(); err != nil {
		m.mu.Unlock()
		_ = os.RemoveAll(dir)
		result := shimwire.LauncherStartResult{ID: command.ID, Error: fmt.Sprintf("start child: %v", err)}
		m.remember(command.ID, body, result)
		return result
	}
	active := &child{generation: command.Generation, pid: process.cmd.Process.Pid, dir: dir, status: process.status, output: process.output, done: make(chan struct{})}
	m.child = active
	result := shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation}
	m.requests[command.ID] = request{body: body, result: result}
	m.mu.Unlock()
	go m.wait(process.cmd, active)
	_ = m.state()
	return result
}

func (m *manager) remember(id string, body []byte, result shimwire.Frame) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[id] = request{body: body, result: result}
}

// command is one generation's process: in an issue pod's container, role-init as the init of
// this role's new user, mount, PID and IPC namespaces, with the pipe it reports the shim's own
// wait status on and prefixed writers of the generation's own; in the controller's pod, the shim
// itself in the container's own namespaces.
func (m *manager) command(start shimwire.LauncherStart, env []string) (*roleProcess, error) {
	if m.namespaced {
		stdout, stderr := m.log.writers(m.role)
		process, err := newRoleProcess(RoleInitConfig{
			RolesDir: m.cfg.RolesDir, Role: m.role, Generation: start.Generation, Home: roleHome,
			HomeMounts: roleHomeMounts, FixedRoot: roleFixedRoot, Argv: start.Argv,
		}, env, stdout, stderr)
		if err != nil {
			return nil, err
		}
		process.output = []io.Writer{stdout, stderr}
		return process, nil
	}
	cmd := exec.Command(start.Argv[0], start.Argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = m.stdout, m.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &roleProcess{cmd: cmd}, nil
}

// wait reports the generation's exit once its process is gone. A role's exit is the shim's own:
// its namespace init reports the status it waited on, and only an init killed before the shim
// ended — or one that never started it — reports its own. Once the process and its output copies are done, the last line the generation left
// unfinished is written out, so it is neither lost nor joined to the next generation's first.
func (m *manager) wait(cmd *exec.Cmd, active *child) {
	var reported *syscall.WaitStatus
	if active.status != nil {
		raw, _ := io.ReadAll(active.status)
		_ = active.status.Close()
		reported = parseStatus(raw)
	}
	if err := cmd.Wait(); err != nil && cmd.ProcessState == nil {
		m.logf("launcher: wait for generation %d: %v", active.generation, err)
	}
	for _, output := range active.output {
		if w, ok := output.(*prefixWriter); ok {
			w.flush()
		}
	}
	if !m.namespaced {
		m.cleanExitedGeneration()
	}
	exit := exitOf(cmd.ProcessState, active.generation)
	if reported != nil {
		exit = exitOfStatus(*reported, active.generation)
	}
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
		m.remember(command.ID, body, result)
		return result
	}
	if active.generation != command.Generation {
		result := shimwire.LauncherStopResult{ID: command.ID, Error: fmt.Sprintf("generation %d is running, not %d", active.generation, command.Generation)}
		m.remember(command.ID, body, result)
		return result
	}
	if err := m.endGeneration(active, time.Duration(command.GraceMs)*time.Millisecond); err != nil {
		result := shimwire.LauncherStopResult{ID: command.ID, Error: err.Error()}
		m.remember(command.ID, body, result)
		return result
	}
	result := shimwire.LauncherStopResult{ID: command.ID, OK: true}
	m.remember(command.ID, body, result)
	return result
}

// stopActive ends this role's generation at the container's own stop. A failure there is this
// role's alone in an issue pod's container: PID 1 serves every other role, so it is logged
// rather than thrown, where the controller pod's single role still fails its container loudly.
func (m *manager) stopActive() {
	m.mu.Lock()
	active := m.child
	m.mu.Unlock()
	if active == nil {
		return
	}
	if err := m.endGeneration(active, m.cfg.StopGrace); err != nil {
		if !m.namespaced {
			panic(err)
		}
		m.logf("launcher: stop role %s: %v", m.role, err)
	}
}

// generationDir is where generation's credentials live under the private directory.
func generationDir(private string, generation uint64) string {
	return filepath.Join(private, shimwire.GenerationDir(generation))
}

// writeFiles materializes command's credentials in a fresh directory for its generation. No child
// runs when it is called, so everything already in the private directory — an earlier
// generation's credentials a crashed launcher never removed, or a directory, file or symlink the
// previous generation's agent planted toward the shared workspace — is removed without being
// followed; the directory is made owner-only and each file is created exclusively without
// following a link, so no credential is written anywhere but that directory. An error names the
// file, never its bytes.
func (m *manager) writeFiles(command shimwire.LauncherStart) (string, error) {
	entries, err := os.ReadDir(m.private)
	if err != nil {
		return "", fmt.Errorf("read the private directory: %v", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(m.private, entry.Name())); err != nil {
			return "", fmt.Errorf("clear %s from the private directory: %v", entry.Name(), err)
		}
	}
	dir := generationDir(m.private, command.Generation)
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

// roleLog is the one container log an issue pod's roles share: every line a role's launcher or
// its child writes carries that role's name, since `kubectl logs` shows one log for the pod's
// roles where one container per role showed six. Whole lines are written under one lock, so two
// roles never interleave inside a line. Each writer holds its own unfinished line, so the
// launcher's lines for a role and each generation of its child are written through writers of
// their own, and none is ever joined to another's unfinished line.
type roleLog struct {
	mu             sync.Mutex
	stdout, stderr io.Writer
}

// writers are a new pair of role's own stdout and stderr: nil where the launcher was given none,
// which exec.Cmd turns into the null device for the child.
func (l *roleLog) writers(role string) (stdout, stderr io.Writer) {
	if l.stdout != nil {
		stdout = &prefixWriter{log: l, out: l.stdout, prefix: role + ": "}
	}
	if l.stderr != nil {
		stderr = &prefixWriter{log: l, out: l.stderr, prefix: role + ": "}
	}
	return stdout, stderr
}

// linef writes one whole line of the launcher's own, no role's, so it carries no prefix: the
// daemon quotes such lines with every role's when the container ends (sandbox's containerLines).
func (l *roleLog) linef(format string, args ...any) {
	if l.stderr == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.stderr, format+"\n", args...)
}

// maxLogLine bounds how much of one unfinished line a role can make PID 1 hold: a longer line is
// written out as a line of its own at that length, and the rest follows behind the prefix again.
const maxLogLine = 64 * 1024

// prefixWriter holds back a partial line until its newline arrives, so a prefix only ever
// precedes the start of a line. It never fails its caller: a line the container's log refuses is
// dropped, since it has nowhere else to go, and a role whose output could not be copied would
// block on a full pipe or lose its output for good.
type prefixWriter struct {
	log     *roleLog
	out     io.Writer
	prefix  string
	partial []byte
	// line is the buffer each prefixed line is built in, reused from one line to the next.
	line []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	w.partial = append(w.partial, p...)
	start := 0
	for {
		rest := w.partial[start:]
		end := bytes.IndexByte(rest, '\n')
		switch {
		case end >= 0:
			w.emit(rest[:end+1], false)
			start += end + 1
		case len(rest) >= maxLogLine:
			w.emit(rest[:maxLogLine], true)
			start += maxLogLine
		default:
			w.partial = w.partial[:copy(w.partial, rest)]
			return len(p), nil
		}
	}
}

// emit writes one line behind the role's prefix, ending a line cut at maxLogLine itself.
func (w *prefixWriter) emit(line []byte, cut bool) {
	w.line = append(append(w.line[:0], w.prefix...), line...)
	if cut {
		w.line = append(w.line, '\n')
	}
	_, _ = w.out.Write(w.line)
}

// flush writes the unfinished line the writer holds, behind its prefix and ended as a line of its
// own: called once the generation writing to it is gone and its output copied, so what it wrote
// last without a newline is neither lost nor joined to anything written after.
func (w *prefixWriter) flush() {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	if len(w.partial) > 0 {
		w.emit(w.partial, true)
		w.partial = w.partial[:0]
	}
}

// resumeLookupTimeout bounds the session table lookup a resume's start waits on, retries included;
// a test shortens it. It sits well inside the daemon's wait on a launcher's start, its boot timeout
// plus its stop grace (130 s by default: runtime/sandbox's relaunch), so a lookup that gives up
// answers before the daemon stops waiting.
var resumeLookupTimeout = 30 * time.Second

// resumeLookupBackoff is the wait before each retry of a session table lookup that could not reach
// the database, doubling up to its last value until resumeLookupTimeout; a test shortens it.
var resumeLookupBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// resumable refuses a resume of a session nothing holds, so it is a launch failure and never a fresh
// agent: Oh My Pi starts a new session at a --resume path it finds empty. It looks where the child's
// Oh My Pi will, by the storage the child's environment names: the session table when
// OMP_SESSION_STORAGE is sql (the runtime's runtime.kubernetes.session_store postgres), through the
// URL file OMP_SESSION_SQL_DSN_FILE names, and otherwise the session file on the issue's volume.
//
// It also reports whether the workspace the child works in, LEGION_WORKSPACE, was recreated since the
// session was last written: provisioning recorded its creation later than that write
// (workspace.Created), so the agent's last turns ran in a workspace that is gone and this one holds
// only what was pushed. A workspace with no record, or a child with no workspace (the controller),
// was not. A record that cannot be read, which an init container killed mid-write can leave, only
// decides that notice: it is written to log and taken as no record, and the resume goes ahead.
func resumable(env []string, file, onDisk string, log io.Writer) (recreated bool, err error) {
	written, err := sessionWritten(env, file, onDisk)
	if err != nil {
		return false, err
	}
	dir := envValue(env, "LEGION_WORKSPACE")
	if dir == "" {
		return false, nil
	}
	created, ok, err := workspace.Created(dir)
	if err != nil {
		if log != nil {
			fmt.Fprintf(log, "legion launcher: resume session %s: %v; the workspace is taken as not recreated\n", file, err)
		}
		return false, nil
	}
	return ok && written.Before(created), nil
}

// sessionWritten is when the session a resume names was last written, from where the child's Oh My
// Pi keeps it (resumable), refusing a session nothing holds. A session file is read at onDisk, where
// this process finds the path the child names (file): in an issue pod the role's home is its own
// overlay, and the volume holding its sessions is mounted under the home mounts outside it
// (containerPath). A session table it cannot reach is asked again after each of
// resumeLookupBackoff's waits until resumeLookupTimeout, the URL file read afresh each time, as a
// database that is briefly unreachable must not fail the launch; a session the table answers it does
// not hold, or a URL file that is missing or empty, is refused at once.
func sessionWritten(env []string, file, onDisk string) (time.Time, error) {
	if strings.TrimSpace(envValue(env, ompsessions.StorageVariable)) != ompsessions.SQLStorage {
		info, err := os.Stat(onDisk)
		if err != nil {
			// The refusal names the file as the role knows it, whichever path held it here.
			var path *fs.PathError
			if errors.As(err, &path) {
				path.Path = file
			}
			return time.Time{}, fmt.Errorf("resume session file %s: %v", file, err)
		}
		return info.ModTime(), nil
	}
	dsnFile := strings.TrimSpace(envValue(env, ompsessions.DSNFileVariable))
	if dsnFile == "" {
		return time.Time{}, fmt.Errorf("resume session %s: %s is %s and %s names no file", file, ompsessions.StorageVariable, ompsessions.SQLStorage, ompsessions.DSNFileVariable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeLookupTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		dsn, err := ompsessions.ReadDSN(dsnFile)
		if err != nil {
			return time.Time{}, fmt.Errorf("resume session %s: %v", file, err)
		}
		written, found, err := lookupWritten(ctx, dsn, file)
		switch {
		case err == nil && found:
			return written, nil
		case err == nil:
			return time.Time{}, fmt.Errorf("resume session %s: the session table holds no such session", file)
		}
		wait := resumeLookupBackoff[min(attempt, len(resumeLookupBackoff)-1)]
		select {
		case <-ctx.Done():
			return time.Time{}, fmt.Errorf("resume session %s: %v (after %d attempts in %s)", file, err, attempt+1, resumeLookupTimeout)
		case <-time.After(wait):
		}
	}
}

// lookupWritten is one session table lookup of file in the database at dsn.
func lookupWritten(ctx context.Context, dsn, file string) (time.Time, bool, error) {
	conn, err := ompsessions.ConnectURL(ctx, dsn)
	if err != nil {
		return time.Time{}, false, err
	}
	defer conn.Close(context.Background())
	return ompsessions.Written(ctx, conn, file)
}

// envValue is name's value in env, the last entry naming it, "" when none does.
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(env[i], name+"="); ok {
			return value
		}
	}
	return ""
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

// exitOf is the exit a process's own wait status reports. A wait that produced no status reports
// code -1, which no process exits with: the exit is unknown, never a clean 0.
func exitOf(state *os.ProcessState, generation uint64) shimwire.LauncherExit {
	if state != nil {
		if status, ok := state.Sys().(syscall.WaitStatus); ok {
			return exitOfStatus(status, generation)
		}
	}
	return shimwire.LauncherExit{Generation: generation, Code: -1}
}

func randomID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}
