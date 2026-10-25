// Package launcher runs one role container's long-lived process supervisor. It connects to the
// daemon independently from the child worker shim, reports actual child state after every
// reconnect, and owns exactly one worker-shim process group at a time.
package launcher

import (
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

// Config is the role-private launcher configuration. Token is read from the role-private
// projected Secret before Run is called. PrivateDir is the role container's own memory-backed
// directory, which no other container mounts: each generation's credentials live in a fresh
// `g<generation>` directory under it for exactly that generation's lifetime.
type Config struct {
	Connect, Token, Sandbox, Role, PodUID, PrivateDir string
	Stdout, Stderr                                    io.Writer
	// StopGrace is the container shutdown and natural-exit descendant cleanup budget.
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
// Only ctx's end (the container's SIGTERM) stops the child the launcher owns.
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
	m := &manager{cfg: cfg, requests: map[string]request{}}
	if os.Getpid() == 1 {
		exits := make(chan os.Signal, 1)
		signal.Notify(exits, syscall.SIGCHLD)
		defer signal.Stop(exits)
		go m.reapOrphans(ctx, exits)
	}
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
// state, then start and stop commands until the connection ends. The acknowledgement is logged as
// one line, so a launcher the daemon accepted reads apart from one still dialing.
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
	reader := shimwire.NewReader(conn)
	frame, err := readFrame(reader)
	if err != nil {
		return fmt.Errorf("hello acknowledgement: %w", err)
	}
	if _, ok := frame.(shimwire.LauncherHelloAck); !ok {
		return fmt.Errorf("hello acknowledgement: got %T, want launcher_hello_ack", frame)
	}
	if m.cfg.Stderr != nil {
		fmt.Fprintf(m.cfg.Stderr, "launcher: connected to the daemon at %s as launcher %s (sandbox %s, role %s, pod %s)\n",
			address, launcherID, m.cfg.Sandbox, m.cfg.Role, m.cfg.PodUID)
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

type child struct {
	generation uint64
	pid        int
	dir        string
	// proxy is the generation's session database proxy, nil when no session database is
	// configured. It belongs to the generation and stops with it.
	proxy *sessionproxy.Proxy
	done  chan struct{}
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
	recreated := false
	if command.ResumeFile != "" {
		if recreated, err = resumable(env, command.ResumeFile, clientURL, m.cfg.Stderr); err != nil {
			endSession(proxy, dir)
			return refuse(err.Error())
		}
	}
	env = mergeEnv(env, []string{WorkspaceRecreatedVariable + "=" + strconv.FormatBool(recreated)})
	cmd := exec.Command(command.Argv[0], command.Argv[1:]...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = m.cfg.Stdout, m.cfg.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The orphan reaper must see this PID as the direct child before it can observe its exit.
	m.mu.Lock()
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		endSession(proxy, dir)
		return refuse(fmt.Sprintf("start child: %v", err))
	}
	active := &child{generation: command.Generation, pid: cmd.Process.Pid, dir: dir, proxy: proxy, done: make(chan struct{})}
	m.child = active
	result := shimwire.LauncherStartResult{ID: command.ID, OK: true, RunningGeneration: command.Generation}
	m.requests[command.ID] = request{body: body, result: result}
	m.mu.Unlock()
	go m.wait(cmd, active)
	_ = m.state()
	return result
}

func (m *manager) remember(id string, body []byte, result shimwire.Frame) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[id] = request{body: body, result: result}
}

func (m *manager) wait(cmd *exec.Cmd, active *child) {
	err := cmd.Wait()
	m.cleanExitedGeneration()
	exit := exitOf(err, active.generation)
	// The generation's proxy stops before its directory goes, so nothing is left listening over a
	// socket that has been removed, and between generations nothing is bound at all.
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

func (m *manager) stopActive() {
	m.mu.Lock()
	active := m.child
	m.mu.Unlock()
	if active == nil {
		return
	}
	if err := m.endGeneration(active, m.cfg.StopGrace); err != nil {
		panic(err)
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
		if err := writeCredential(filepath.Join(dir, name), content); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("write credential %s for generation %d: %v", name, command.Generation, err)
		}
	}
	return dir, nil
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
func (m *manager) startSession(ctx context.Context, generation uint64, dir string) (*sessionproxy.Proxy, string, error) {
	if m.cfg.SessionDatabase == (ompsessions.Database{}) {
		return nil, "", nil
	}
	endpoint := ompsessions.Endpoint{SocketDir: dir, Database: m.cfg.SessionDatabase.Name, User: m.cfg.SessionDatabase.User}
	if err := writeCredential(filepath.Join(dir, ompsessions.DSNFileName), endpoint.AgentURL()); err != nil {
		return nil, "", fmt.Errorf("write the session database URL for generation %d: %v", generation, err)
	}
	proxy, err := sessionproxy.Start(ctx, sessionproxy.Config{
		Socket: endpoint.Socket(), Instance: m.cfg.SessionDatabase.Instance, NewDialer: m.cfg.SessionDialer, Log: m.cfg.Stderr,
	})
	if err != nil {
		return nil, "", fmt.Errorf("the session database proxy for generation %d: %v", generation, err)
	}
	return proxy, endpoint.ClientURL(), nil
}

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
// OMP_SESSION_STORAGE is sql (the runtime's runtime.kubernetes.session_store postgres and
// cloudsql-iam), through clientURL when the generation has a session database of its own and
// otherwise the URL file OMP_SESSION_SQL_DSN_FILE names, and otherwise the session file on the
// issue's volume.
//
// It also reports whether the workspace the child works in, LEGION_WORKSPACE, was recreated since the
// session was last written: provisioning recorded its creation later than that write
// (workspace.Created), so the agent's last turns ran in a workspace that is gone and this one holds
// only what was pushed. A workspace with no record, or a child with no workspace (the controller),
// was not. A record that cannot be read, which an init container killed mid-write can leave, only
// decides that notice: it is written to log and taken as no record, and the resume goes ahead.
func resumable(env []string, file, clientURL string, log io.Writer) (recreated bool, err error) {
	written, err := sessionWritten(env, file, clientURL)
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
// Pi keeps it (resumable), refusing a session nothing holds. clientURL is the generation's own
// session database through the socket its proxy has already bound; with none it is "" and the table
// is reached through the URL file the child's environment points at, read afresh at each attempt,
// since that file is a projection something else may replace. A session table it cannot reach is
// asked again after each of resumeLookupBackoff's waits until resumeLookupTimeout, as a database
// that is briefly unreachable must not fail the launch; a session the table answers it does not
// hold, or a URL file that is missing or empty, is refused at once.
func sessionWritten(env []string, file, clientURL string) (time.Time, error) {
	if strings.TrimSpace(envValue(env, ompsessions.StorageVariable)) != ompsessions.SQLStorage {
		info, err := os.Stat(file)
		if err != nil {
			return time.Time{}, fmt.Errorf("resume session file %s: %v", file, err)
		}
		return info.ModTime(), nil
	}
	dsnFile := ""
	if clientURL == "" {
		if dsnFile = strings.TrimSpace(envValue(env, ompsessions.DSNFileVariable)); dsnFile == "" {
			return time.Time{}, fmt.Errorf("resume session %s: %s is %s and %s names no file", file, ompsessions.StorageVariable, ompsessions.SQLStorage, ompsessions.DSNFileVariable)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeLookupTimeout)
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
