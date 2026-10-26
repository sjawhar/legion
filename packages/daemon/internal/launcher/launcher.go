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
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
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
	// SessionDatabase is the Cloud SQL database every generation keeps its Oh My Pi session in,
	// which the launcher proxies to over the pod's own cloud identity (internal/sessionproxy). The
	// zero value is every other session store: no proxy, and the child's session storage is
	// whatever its environment names.
	SessionDatabase ompsessions.Database
	// SessionDialer builds that proxy's upstream dialer. Nil is the Cloud SQL connector's, which
	// is the only one a pod uses; a test passes a fake.
	SessionDialer func(ctx context.Context) (sessionproxy.Dialer, error)
}

// reconnectDelay is the pause between daemon connections. A lost connection never stops the
// child: a daemon restart re-adopts the role process the launcher still owns.
const reconnectDelay = time.Second

// Run serves launcher commands until ctx ends. A stopped child is reported, never restarted by
// the launcher: the daemon owns generations and decides whether a new generation should start.
// Only ctx's end (the container's SIGTERM) stops the children the launcher owns, and in an issue
// pod's container it stops every role's at once, each with the whole stop grace. Run returns only
// once each of those generations has ended (stopActive): its child given its stop grace, and its
// session database proxy, which served it through that grace, closed.
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
	// PID 1 adopts every orphan of its PID namespace and must reap it. The controller's pod runs
	// one role, whose processes are this namespace's; an issue pod's roles each have a namespace
	// of their own, whose init reaps theirs, but anything else started in the container (a
	// `kubectl exec` and what it leaves behind) is this process's. The reaper never waits on a
	// generation's own process, whose exit its manager reads.
	if os.Getpid() == 1 {
		exits := make(chan os.Signal, 1)
		signal.Notify(exits, syscall.SIGCHLD)
		defer signal.Stop(exits)
		go reapOrphans(ctx, exits, managers)
	}
	if cfg.RolesDir == "" {
		return managers[0].serveUntil(ctx, address)
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
			answer = m.start(ctx, command)
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
		return c.validateSessionDatabase()
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
	return c.validateSessionDatabase()
}

// validateSessionDatabase refuses a session database named only in part: the instance, the database
// in it and the login together are what opens it, so a generation started without one of them would
// proxy to nothing. Naming none of them is the other stores, which need no proxy.
func (c Config) validateSessionDatabase() error {
	var missing []string
	for _, named := range []struct{ flag, value string }{
		{"--session-instance", c.SessionDatabase.Instance},
		{"--session-database", c.SessionDatabase.Name},
		{"--session-user", c.SessionDatabase.User},
	} {
		if named.value == "" {
			missing = append(missing, named.flag)
		}
	}
	if len(missing) == 0 || len(missing) == 3 {
		return nil
	}
	return fmt.Errorf("launcher: the session database is named in part: no %s", strings.Join(missing, ", no "))
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
	// process is the generation's own handle, through which the launcher signals it: once its
	// wait has reaped it, a signal through the handle reaches nothing (Go holds the process's
	// pidfd), where one by pid could reach whatever process the kernel gave that number since.
	process *os.Process
	dir     string
	status  *os.File
	// output is the generation's own prefixed stdout and stderr in an issue pod's container,
	// whose unfinished last lines wait flushes once the generation's process is gone.
	output []io.Writer
	// proxy is the generation's session database proxy, nil when no session database is
	// configured. It belongs to the generation: it outlives the child it serves, through the
	// child's stop grace however the stop arrives, and is closed once the child has exited (wait).
	proxy *sessionproxy.Proxy
	// keeper keeps a Claude Code child's session in the session table, nil for any other child
	// (claudeKeeper). It saves until the child has exited, then makes its last save before the
	// proxy closes (wait).
	keeper *claudeKeeper
	// stopBy is when the stop under way, if any, gives up on the child: the end of its grace
	// (endGeneration), which bounds the keeper's last save. Guarded by manager.mu.
	stopBy time.Time
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

// start runs command's generation. ctx is the container's: once it has ended (the container's
// SIGTERM) no generation starts, since one started then would be given the whole stop grace from
// that moment and outlast the pod's own; the session lookup a resume waits on ends with it too.
func (m *manager) start(ctx context.Context, command shimwire.LauncherStart) shimwire.LauncherStartResult {
	if err := command.Validate(); err != nil {
		return shimwire.LauncherStartResult{ID: command.ID, Error: err.Error()}
	}
	body, _ := json.Marshal(command)
	// refuse is how a start that cannot run answers: the refusal is remembered under the command's
	// id, so a retry of that id is answered with it rather than tried again.
	refuse := func(message string) shimwire.LauncherStartResult {
		result := shimwire.LauncherStartResult{ID: command.ID, Error: message}
		m.remember(command.ID, body, result)
		return result
	}
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
	// The generation's own directory comes first, since everything else of the generation lives in
	// it: the session database's connection file and the socket its proxy binds, which the resume
	// lookup reaches the table through and which the child must find already bound.
	dir, err := m.writeFiles(command)
	if err != nil {
		return refuse(err.Error())
	}
	proxy, clientURL, err := m.startSession(ctx, command.Generation, dir)
	if err != nil {
		endSession(proxy, dir)
		return refuse(err.Error())
	}
	// A Claude Code child's session is kept in the session table (claudeKeeper): the order is
	// the resume's lookup, which decides the recreated-workspace notice from the row's last save,
	// then the restore of the session's files from their rows, then the child, then the saves.
	keeper, err := newClaudeKeeper(env, command.ResumeFile, clientURL, m.stderr)
	if err != nil {
		endSession(proxy, dir)
		return refuse(err.Error())
	}
	recreated := false
	if command.ResumeFile != "" {
		if recreated, err = resumable(ctx, env, command.ResumeFile, clientURL, m.stderr); err != nil {
			endSession(proxy, dir)
			return refuse(err.Error())
		}
	}
	if keeper != nil {
		if err := keeper.open(ctx, command.ResumeFile != ""); err != nil {
			keeper.close()
			endSession(proxy, dir)
			return refuse(err.Error())
		}
	}
	env = mergeEnv(env, []string{WorkspaceRecreatedVariable + "=" + strconv.FormatBool(recreated)})
	process, err := m.command(command, env)
	if err != nil {
		if keeper != nil {
			keeper.close()
		}
		endSession(proxy, dir)
		return refuse(err.Error())
	}
	// The orphan reaper must see this PID as the direct child before it can observe its exit, and
	// stopActive, which takes the lock, either sees this generation or finds the container
	// stopping here and nothing started.
	m.mu.Lock()
	if ctx.Err() != nil {
		m.mu.Unlock()
		if keeper != nil {
			keeper.close()
		}
		endSession(proxy, dir)
		if process.status != nil {
			_ = process.status.Close()
			_ = process.write.Close()
		}
		return refuse("the launcher's container is stopping; no generation starts")
	}
	if err := process.start(); err != nil {
		m.mu.Unlock()
		if keeper != nil {
			keeper.close()
		}
		endSession(proxy, dir)
		return refuse(fmt.Sprintf("start child: %v", err))
	}
	active := &child{generation: command.Generation, pid: process.cmd.Process.Pid, process: process.cmd.Process, dir: dir, status: process.status, output: process.output, proxy: proxy, keeper: keeper, done: make(chan struct{})}
	m.child = active
	result := shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation}
	m.requests[command.ID] = request{body: body, result: result}
	m.mu.Unlock()
	if keeper != nil {
		go keeper.run()
	}
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
// ended — or one that never started it — reports its own. An init that exited with
// RoleInitSetupFailedExit having reported nothing failed before it started the shim (setting up
// the role's namespaces, or starting the shim), which is reported as
// shimwire.LauncherExitSetupFailed rather than as an exit code a shim could have exited with; any
// other exit of an init that reported nothing (a panic's 2, say) is reported as that exit. Once
// the process and its output copies are done, the last line the generation left unfinished is
// written out, so it is neither lost nor joined to the next generation's first.
//
// The generation's session database proxy is closed only once its process is gone, so the child is
// served through the whole of its stop grace however the stop arrives, the daemon's stop or the
// container's own SIGTERM; and after every process of the generation has ended (its PID namespace's
// init gone in an issue pod, cleanExitedGeneration in the controller's PID 1), and so every
// connection they held. A Claude Code child's keeper makes its last save between the two
// (claudeKeeper.finish), until the stop's grace less the proxy's drain is over, so that save and
// the keeper's own connection are done before the proxy closes. Close is bounded by sessionDrain
// however many holders remain: off PID 1 cleanExitedGeneration returns at once and a socket holder
// outside the child's process group outlives it, and Close closes that holder's connection once
// the drain is over rather than wait on it. It is closed before the generation's directory goes, so
// nothing is left listening over a socket that has been removed, and between generations nothing is
// bound at all. active.done is closed last, so endGeneration, and with it Run's stopActive, returns
// only once the proxy is closed.
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
	switch {
	case reported != nil:
		exit = exitOfStatus(*reported, active.generation)
	case m.namespaced && cmd.ProcessState != nil && cmd.ProcessState.Exited() && cmd.ProcessState.ExitCode() == RoleInitSetupFailedExit:
		exit = shimwire.LauncherExit{Generation: active.generation, Code: shimwire.LauncherExitSetupFailed}
		m.logf("launcher: generation %d of role %s: role-init failed before it started the shim; its own lines above say why",
			active.generation, m.role)
	}
	if active.keeper != nil {
		m.mu.Lock()
		end := active.stopBy
		m.mu.Unlock()
		// A child that exited on its own is given the whole grace a stop would have given it.
		if end.IsZero() {
			end = time.Now().Add(m.cfg.StopGrace)
		}
		active.keeper.finish(end.Add(-sessionDrain))
	}
	if active.proxy != nil {
		_ = active.proxy.Close()
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
// followed; the directory is made owner-only and each file is written as writeCredential writes
// one, so no credential is written anywhere but that directory. An error names the file, never its
// bytes.
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
		if err := writeCredential(filepath.Join(dir, name), content); err != nil {
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

// writeCredential writes content to path the one way anything of a generation's is written: created
// exclusively, owner-only, and never following a link, so a credential lands in the generation's
// own directory and nowhere else, and never through a file or symlink already at that path. The
// error it returns names neither the file nor its bytes; each caller names the file itself.
func writeCredential(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(content)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// startSession gives a generation its session database, when one is configured: the connection URL
// the child's Oh My Pi reads, written into the generation's own directory the way writeFiles writes
// a credential, and the proxy bound on the socket beside it, which has proven one upstream
// connection before Start returns. The URL it answers with is that same endpoint for the launcher's
// own pgx client, which spells a unix socket differently from the client the agent runs
// (ompsessions.Endpoint); with no session database it is "", and the launcher looks a resume up
// through the pointer the child's environment carries instead. A failure leaves nothing bound and
// names the generation; its caller removes the directory.
//
// In an issue pod's container the launcher and the role see the generation's directory at two
// paths: the launcher binds the socket at dir, under the role's own private directory of the roles
// directory, and the role's namespaces show that directory at the fixed root (role-init binds it
// back there), so the URL the agent reads names the socket where the agent finds it. The role's
// processes run as the launcher's own user, so the socket is theirs to connect to.
//
// ctx bounds the proxy's start, so a start still in progress when the container is told to stop is
// abandoned, and its proof dial is bounded besides by proofTimeout, so an instance that never
// answers is refused while the daemon still waits for the answer. A started proxy ignores ctx: it
// serves until the generation's own end closes it (wait), which lets it serve the child through its
// stop grace after ctx has ended.
func (m *manager) startSession(ctx context.Context, generation uint64, dir string) (*sessionproxy.Proxy, string, error) {
	if m.cfg.SessionDatabase == (ompsessions.Database{}) {
		return nil, "", nil
	}
	local := ompsessions.Endpoint{SocketDir: dir, Database: m.cfg.SessionDatabase.Name, User: m.cfg.SessionDatabase.User}
	agent := local
	if m.namespaced {
		agent.SocketDir = generationDir(filepath.Join(roleFixedRoot, RolePrivateDir), generation)
	}
	if err := writeCredential(filepath.Join(dir, ompsessions.DSNFileName), agent.AgentURL()); err != nil {
		return nil, "", fmt.Errorf("write the session database URL for generation %d: %v", generation, err)
	}
	proxy, err := sessionproxy.Start(ctx, sessionproxy.Config{
		Socket: local.Socket(), Instance: m.cfg.SessionDatabase.Instance, NewDialer: m.cfg.SessionDialer,
		ProofTimeout: m.proofTimeout(), Drain: sessionDrain, Log: m.stderr,
	})
	if err != nil {
		return nil, "", fmt.Errorf("the session database proxy for generation %d: %v", generation, err)
	}
	return proxy, local.ClientURL(), nil
}

// proofTimeout bounds a generation's proof dial (sessionproxy.Config.ProofTimeout) at half the stop
// grace, which Run has made positive. The daemon's wait on a start ends at the launcher's connection
// deadline plus its termination grace (runtime/sandbox's relaunch), and that grace is the
// --stop-grace it hands the launcher (runtime/sandbox's manifest), so the daemon waits at least
// StopGrace past the launcher's connection, however late it connected. Half of it bounds the
// proof; the other half is left for the rest of the start and its answer.
func (m *manager) proofTimeout() time.Duration { return m.cfg.StopGrace / 2 }

// sessionDrain is how long a generation's proxy lets connections still open at its Close finish
// (sessionproxy.Config.Drain) before it closes them. Close runs only once the generation's child
// has exited, and in a pod's PID 1 once every process of the generation is gone, whose connections
// have then already closed, so anything still connected is no part of the agent's work. One second
// lets a copy already under way deliver what it holds, and no holder outside the generation can
// keep the generation, the next one or Run waiting longer.
const sessionDrain = time.Second

// endSession ends a generation that will not run after all: its proxy stops and its directory goes,
// so no child ever starts against a half-made generation and nothing of it is left bound or on
// disk. wait does the same for a generation that ran.
func endSession(proxy *sessionproxy.Proxy, dir string) {
	if proxy != nil {
		_ = proxy.Close()
	}
	_ = os.RemoveAll(dir)
}

// resumeLookupTimeout bounds the session table lookup a resume's start waits on, retries included;
// a test shortens it. A start's whole budget is the proof dial, at most proofTimeout (StopGrace/2),
// then this lookup: 35 s at the daemon's defaults, inside its wait on the start, its boot timeout
// plus its stop grace (120 s + 10 s: runtime/sandbox's relaunch), whenever the launcher is connected
// as that wait begins, which is every relaunch into a running pod. Only a launcher that connects late
// in a new pod's boot window has less: at least the grace remains, and the proof's half of it is
// still inside.
var resumeLookupTimeout = 30 * time.Second

// resumeLookupBackoff is the wait before each retry of a session table lookup that could not reach
// the database, doubling up to its last value until resumeLookupTimeout; a test shortens it.
var resumeLookupBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}

// resumable refuses a resume of a session nothing holds, so it is a launch failure and never a fresh
// agent: Oh My Pi starts a new session at a --resume path it finds empty. It looks where the child's
// Oh My Pi will, in the session table (the runtime's runtime.kubernetes.session_store, which must
// keep sessions in a database: no issue disk holds one), through clientURL when the generation has
// a session database of its own and otherwise the URL file OMP_SESSION_SQL_DSN_FILE names. A
// Claude Code resume's file is its transcript's path, `<CLAUDE_CONFIG_DIR>/projects/<dir>/<id>.jsonl`
// (the session a claude-envoy claim records), which is the very key its launcher kept the
// transcript's row under (claudeKeeper): the row's mtime_ms is that file's mtime at its last save.
//
// It also reports whether the workspace the child works in, LEGION_WORKSPACE, was recreated since the
// session was last written: provisioning recorded its creation later than that write
// (workspace.Created), so the agent's last turns ran in a workspace that is gone and this one holds
// only what was pushed. A workspace with no record, or a child with no workspace (the controller),
// was not. A record that cannot be read, which an init container killed mid-write can leave, only
// decides that notice: it is written to log and taken as no record, and the resume goes ahead.
func resumable(ctx context.Context, env []string, file, clientURL string, log io.Writer) (recreated bool, err error) {
	written, err := sessionWritten(ctx, env, file, clientURL)
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

// sessionWritten is when the session a resume names was last written, read from the session table
// the child's Oh My Pi keeps it in (resumable), refusing a session nothing holds. clientURL is the
// generation's own session database through the socket its proxy has already bound; with none it is
// "" and the table is reached through the URL file the child's environment points at, read afresh
// at each attempt, since that file is a projection something else may replace. A session table it
// cannot reach is asked again after each of resumeLookupBackoff's waits until resumeLookupTimeout,
// as a database that is briefly unreachable must not fail the launch; a session the table answers
// it does not hold, or a URL file that is missing or empty, is refused at once. parent's end (the
// container's stop) ends the wait as the timeout does.
func sessionWritten(parent context.Context, env []string, file, clientURL string) (time.Time, error) {
	dsnFile := ""
	if clientURL == "" {
		if dsnFile = strings.TrimSpace(envValue(env, ompsessions.DSNFileVariable)); dsnFile == "" {
			return time.Time{}, fmt.Errorf("resume session %s: %s names no file", file, ompsessions.DSNFileVariable)
		}
	}
	ctx, cancel := context.WithTimeout(parent, resumeLookupTimeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		dsn := clientURL
		if dsnFile != "" {
			var err error
			if dsn, err = ompsessions.ReadDSN(dsnFile); err != nil {
				return time.Time{}, fmt.Errorf("resume session %s: %v", file, err)
			}
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
