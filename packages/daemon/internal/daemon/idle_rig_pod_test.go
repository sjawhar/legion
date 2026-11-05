package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/sessionproxy"
)

// idleRigPods runs the processes of every pod the rig's Kubernetes stand-in reports Ready: one
// real `legion launcher` per role of the pod, started in this process as
// packages/daemon/cmd/legion/launcher_test.go starts the command's launcher (launcher.Run over the
// address the daemon's worker stream bound, with the role's own token), and stopped when the pod
// is deleted.
//
// It is the launcher's single-role form, one Run per role rather than one Run for the whole pod's
// roles directory. An issue pod's container runs the roles-dir form, which starts every role
// generation as the init of new user, mount, PID and IPC namespaces over the worker image's
// layout (`legion launcher role-init`, internal/launcher/roleinit_linux.go), and this container
// has neither that layout nor a way to make its mount points. The daemon's side of the protocol is
// the same either way: one authenticated launcher connection per role, each reporting its own
// child's state.
//
// Each launcher's generations keep their Oh My Pi session in the rig's Postgres, through the
// launcher's own session proxy: the runtime is configured with the cloudsql-iam store, so a
// resume's session lookup goes through the proxy's socket (internal/launcher's startSession and
// resumable) rather than through a providers mount this container cannot have, and the proxy's
// upstream dialer is a plain TCP dial of the test database.
type idleRigPods struct {
	t *testing.T
	// address is the worker stream every launcher dials, tcp://host:port.
	address func() string
	// session is the Cloud SQL session database the launchers proxy to, and dial is its upstream.
	session ompsessions.Database
	dial    func(ctx context.Context) (sessionproxy.Dialer, error)

	mu sync.Mutex
	// running is each live pod's launchers, by pod name.
	running map[string]*idleRigPodProcesses
	// logs is every launcher's own log, for a failure to name what the pod did.
	logs *syncBuffer
	// started counts the launchers started for a pod, by pod name: a pod that came back is a
	// second start, which is what a wake must produce.
	started map[string]int

	dirMu   sync.Mutex
	dirRoot string
	dirNext int
}

// idleRigPodProcesses is one pod's launchers: the context that stops them and the wait that their
// goroutines have ended.
type idleRigPodProcesses struct {
	cancel context.CancelFunc
	done   sync.WaitGroup
	dirs   []string
}

func newIdleRigPods(t *testing.T, address func() string, session ompsessions.Database, dial func(ctx context.Context) (sessionproxy.Dialer, error)) *idleRigPods {
	t.Helper()
	p := &idleRigPods{t: t, address: address, session: session, dial: dial,
		running: map[string]*idleRigPodProcesses{}, logs: &syncBuffer{}, started: map[string]int{}}
	t.Cleanup(p.stopAll)
	return p
}

// podReady starts a launcher for each role of the pod, with that role's own launcher token.
func (p *idleRigPods) podReady(pod, podUID string, roles map[string]string, record func(role, dir string)) {
	p.mu.Lock()
	if _, live := p.running[pod]; live {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	processes := &idleRigPodProcesses{cancel: cancel}
	p.running[pod] = processes
	p.started[pod]++
	p.mu.Unlock()
	for role, token := range roles {
		dir, err := p.privateDir()
		if err != nil {
			p.t.Errorf("make the private directory of %s/%s: %v", pod, role, err)
			continue
		}
		processes.dirs = append(processes.dirs, dir)
		record(role, dir)
		cfg := launcher.Config{
			Connect: p.address(), Token: token, Sandbox: pod, Role: role, PodUID: podUID, PrivateDir: dir,
			Stdout: p.logs, Stderr: p.logs, StopGrace: 10 * time.Second,
			SessionDatabase: p.session, SessionDialer: p.dial,
		}
		processes.done.Add(1)
		go func() {
			defer processes.done.Done()
			if err := launcher.Run(ctx, cfg); err != nil && ctx.Err() == nil {
				fmt.Fprintf(p.logs, "launcher %s/%s: %v\n", pod, role, err)
			}
		}()
	}
}

// privateDir is one launcher's private directory, where its generations' credentials and the
// unix socket of each generation's session database proxy live. The socket is
// `<dir>/g<n>/.s.PGSQL.5432`, 17 bytes under the directory, and a unix socket path is bounded at
// 107 (docs/testing.md), so the directories are kept as short as a temporary directory of about
// 78 bytes leaves room for: one root of `p<pid>` under it, and a counter under that. The
// package's shortTempDir and os.MkdirTemp's own ten-digit suffix are both too long for this one.
func (p *idleRigPods) privateDir() (string, error) {
	p.dirMu.Lock()
	defer p.dirMu.Unlock()
	if p.dirRoot == "" {
		root := filepath.Join(os.TempDir(), fmt.Sprintf("p%d", os.Getpid()%100000))
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		p.t.Cleanup(func() { _ = os.RemoveAll(root) })
		p.dirRoot = root
	}
	p.dirNext++
	dir := filepath.Join(p.dirRoot, strconv.Itoa(p.dirNext))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if len(dir) > 88 {
		return "", fmt.Errorf("the launcher's private directory %s is %d bytes; its session socket would pass the 107-byte limit", dir, len(dir))
	}
	return dir, nil
}

// podGone stops every launcher of the pod and waits for them, as the kubelet's stop of the
// container does: the pod is gone, so nothing of it may still answer the daemon.
func (p *idleRigPods) podGone(pod string) {
	p.mu.Lock()
	processes, live := p.running[pod]
	delete(p.running, pod)
	p.mu.Unlock()
	if !live {
		return
	}
	processes.cancel()
	processes.done.Wait()
	for _, dir := range processes.dirs {
		_ = os.RemoveAll(dir)
	}
}

func (p *idleRigPods) stopAll() {
	p.mu.Lock()
	pods := make([]string, 0, len(p.running))
	for pod := range p.running {
		pods = append(pods, pod)
	}
	p.mu.Unlock()
	for _, pod := range pods {
		p.podGone(pod)
	}
}

// starts is how many times the pod's launchers were started: one per pod the rig saw Ready.
func (p *idleRigPods) starts(pod string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started[pod]
}

// log is every launcher's output so far.
func (p *idleRigPods) log() string { return p.logs.String() }

// idleRigDialer dials the rig's Postgres for the launcher's session proxy: the upstream a pod's
// Cloud SQL connector is in production, here a plain TCP connection to the test database's server.
type idleRigDialer struct{ address string }

func (d idleRigDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.address)
}

func (idleRigDialer) Close() error { return nil }

// idleRigWriteSessionRow records a session file in the rig's session table, as the agent's Oh My
// Pi does when it writes its session: the launcher refuses to resume a session the table does not
// hold (internal/launcher's sessionWritten), so every session the rig resumes has a row.
func idleRigSessionTable() string {
	return `create table if not exists omp_session_files (
		path text primary key,
		mtime_ms bigint not null,
		content bytea not null default ''
	)`
}

// idleRigSessionFile is the session file the rig's stand-in agent of role reports to the daemon,
// the path a resume names. It is the shape a pod's Oh My Pi writes under its own state directory.
func idleRigSessionFile(issue, role string) string {
	return "/legion/sessions/" + issue + "-" + role + ".jsonl"
}

// idleRigLegionTool writes, under dir, the `legion` the pod's launcher runs as the role process:
// a shell script that re-enters this test binary as TestIdleRigRoleProcess with the whole argv
// the launcher was told, in the shape of packages/daemon/internal/capabilities/image_test.go's
// fakeOmp. The role process the real launcher starts in a pod is `legion worker-shim … -- <omp
// argv>`; this stand-in is that whole process, Oh My Pi included: it speaks the worker stream's
// frames, registers with the daemon, reports ready, claims its role on the Envoy listener, and
// records every message it is given.
func idleRigLegionTool(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legion")
	script := "#!/bin/sh\nexec " + shellQuote(self) + " -test.run='^TestIdleRigRoleProcess$' " +
		"-test.timeout=0 -test.count=1 \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the rig's legion stand-in: %v", err)
	}
	return path
}

// shellQuote is one argument as a POSIX shell reads it.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// idleRigAgentTool is the agent command the shim's argv ends with, which the stand-in records and
// never runs: a path that names the agent the pod would have started.
func idleRigAgentTool(t *testing.T, dir string) []string {
	t.Helper()
	path := filepath.Join(dir, "omp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write the rig's omp stand-in: %v", err)
	}
	return []string{path}
}
