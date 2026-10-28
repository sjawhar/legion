package launcher

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/testpg"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// shortenResumeLookup makes a lookup that cannot reach the session table give up in seconds rather
// than the thirty seconds a launch is allowed, so a test that expects an answer fails quickly when
// none comes.
func shortenResumeLookup(t *testing.T) {
	t.Helper()
	savedBackoff, savedTimeout := resumeLookupBackoff, resumeLookupTimeout
	resumeLookupBackoff, resumeLookupTimeout = []time.Duration{100 * time.Millisecond}, 3*time.Second
	t.Cleanup(func() { resumeLookupBackoff, resumeLookupTimeout = savedBackoff, savedTimeout })
}

// Under SQL session storage (either database kind of the runtime's session_store, postgres or
// cloudsql-iam: OMP_SESSION_STORAGE=sql and OMP_SESSION_SQL_DSN_FILE in the generation's
// environment) a resume is held to the session table,
// where the child's Oh My Pi will read it, never to the volume: a session the table does not hold is
// refused before anything starts, even when a file of that name is on disk, and a session the table
// holds starts with no file on disk at all.
func TestStartUnderSQLStorageResumesOnlyASessionTheTableHolds(t *testing.T) {
	_, dsnFile := testpg.DSNFile(t, "legion_launcher_test")
	g := newRig(t)
	marker := filepath.Join(t.TempDir(), "marker")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0001.jsonl")
	start := func(generation uint64) (string, bool) {
		command := g.childStart(generation, marker, nil)
		command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage, ompsessions.DSNFileVariable+"="+dsnFile)
		command.ResumeFile = sessionFile
		got := g.start(command)
		return got.Error, got.OK
	}

	refusal, ok := start(5)
	if ok || !strings.Contains(refusal, "resume session "+sessionFile+": the session table holds no such session") {
		t.Fatalf("start of a session the table does not hold = %q (ok %t), want the refusal naming it", refusal, ok)
	}
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(6); ok || !strings.Contains(refusal, "the session table holds no such session") {
		t.Fatalf("start with the file on disk but not in the table = %q (ok %t), want the table's refusal", refusal, ok)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused resume started the child")
	}

	if err := os.Remove(sessionFile); err != nil {
		t.Fatal(err)
	}
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(7); !ok {
		t.Fatalf("start of a session the table holds = %q, want generation 7 to run", refusal)
	}
	published(t, marker)
	g.stop(7)
}

// A generation with no URL file is refused before it starts, naming the variable: the launcher
// cannot look where Oh My Pi would.
func TestStartRefusesAResumeWithNoURLFile(t *testing.T) {
	g := newRig(t)
	command := g.childStart(3, filepath.Join(t.TempDir(), "marker"), nil)
	command.ResumeFile = "/sessions/absent.jsonl"
	if got := g.start(command); got.OK || !strings.Contains(got.Error, "OMP_SESSION_SQL_DSN_FILE names no file") {
		t.Fatalf("start = %#v, want the refusal naming OMP_SESSION_SQL_DSN_FILE", got)
	}
}

// A session's last write is the table's mtime_ms, which Oh My Pi moves on every write and
// `legion sessions import` sets to the copied file's. A resumed generation is told whether its
// workspace (LEGION_WORKSPACE) was recreated since that write, which the Legion plugin tells the
// agent on its first turn: "true" once provisioning's creation record is later, "false" when it is
// earlier or there is none. A fresh generation resumes nothing and is told "false", and so is one
// with no workspace at all and every generation whatever the container's own environment says, so
// no other value reaches the child. A record that is empty or not an instant, which an init
// container killed mid-write can leave, only decides the notice: the resume starts, told "false",
// and the launcher logs the record.
func TestAResumeIsToldWhetherItsWorkspaceWasRecreatedSinceTheTableWroteItsSession(t *testing.T) {
	t.Setenv(WorkspaceRecreatedVariable, "true")
	_, dsnFile := testpg.DSNFile(t, "legion_launcher_recreated_test")
	g := newRig(t)
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0002.jsonl")
	written := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), written); err != nil {
		t.Fatal(err)
	}
	dir := newWorkspace(t)
	table := []string{ompsessions.DSNFileVariable + "=" + dsnFile}
	env := append([]string{"LEGION_WORKSPACE=" + dir}, table...)
	if got := g.toldRecreated(1, env, sessionFile); got != "false" {
		t.Errorf("a resume in a workspace with no creation record was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(2, env, sessionFile); got != "false" {
		t.Errorf("a resume in a workspace older than the table's last write was told %q, want false", got)
	}
	if err := workspace.RecordCreated(dir, written.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := g.toldRecreated(3, env, sessionFile); got != "true" {
		t.Errorf("a resume in a workspace recreated after the table's last write was told %q, want true", got)
	}
	if got := g.toldRecreated(4, env, ""); got != "false" {
		t.Errorf("a fresh generation was told %q, want false", got)
	}
	if got := g.toldRecreated(5, table, sessionFile); got != "false" {
		t.Errorf("a resume with no workspace was told %q, want false", got)
	}
	for i, record := range []string{"soon\n", ""} {
		if err := os.WriteFile(filepath.Join(dir, ".jj", "legion-created"), []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := g.toldRecreated(uint64(6+i), env, sessionFile); got != "false" {
			t.Errorf("a resume in a workspace whose creation record is %q was told %q, want false", record, got)
		}
	}
	if logged := g.stderr.String(); !strings.Contains(logged, filepath.Join(dir, ".jj", "legion-created")) ||
		!strings.Contains(logged, "the workspace is taken as not recreated") {
		t.Errorf("the launcher logged %q, want the unreadable creation record named", logged)
	}
}

// A session database that cannot be reached for a moment does not fail the launch: the lookup is
// asked again after each backoff wait, reading the URL file afresh, until the table answers. One
// that stays unreachable past resumeLookupTimeout refuses the start, naming how often it asked.
func TestAResumeUnderSQLStorageWaitsOutAnUnreachableSessionDatabase(t *testing.T) {
	dsn, dsnFile := testpg.DSNFile(t, "legion_launcher_retry_test")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0003.jsonl")
	conn, err := ompsessions.Connect(context.Background(), dsnFile)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	shortenResumeLookup(t)
	// writeURL replaces the URL file whole, as a rename does, so a read never sees half of it.
	writeURL := func(url string) {
		staged := dsnFile + ".staged"
		if err := os.WriteFile(staged, []byte(url+"\n"), 0o600); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(staged, dsnFile); err != nil {
			t.Error(err)
		}
	}
	unreachable := "postgres://legion:unused@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"
	writeURL(unreachable)
	env := []string{ompsessions.StorageVariable + "=" + ompsessions.SQLStorage, ompsessions.DSNFileVariable + "=" + dsnFile}
	back := make(chan struct{})
	go func() {
		defer close(back)
		time.Sleep(300 * time.Millisecond)
		writeURL(dsn)
	}()
	if _, err := sessionWritten(context.Background(), env, sessionFile, ""); err != nil {
		t.Fatalf("a lookup whose database came back during the backoff = %v, want the session found", err)
	}
	<-back

	writeURL(unreachable)
	started := time.Now()
	_, err = sessionWritten(context.Background(), env, sessionFile, "")
	if err == nil || !strings.Contains(err.Error(), "resume session "+sessionFile) || !strings.Contains(err.Error(), "attempts in "+resumeLookupTimeout.String()) {
		t.Fatalf("a lookup whose database never came back = %v, want the refusal naming the session and the attempts", err)
	}
	if waited := time.Since(started); waited < resumeLookupTimeout-time.Second {
		t.Fatalf("the lookup gave up after %s, want it to keep asking for %s", waited, resumeLookupTimeout)
	}
}

// sessionDatabaseAt is the test Postgres as a session database a launcher proxies to, with the
// dialer that reaches it as the Cloud SQL connector would. The database and the login are the test
// database's own, since an IAM login exists only in a real project, and its password goes into the
// environment as libpq's PGPASSWORD: it is the only place left for one, the launcher's client
// building its URL from the endpoint alone (ompsessions.Endpoint.ClientURL), which carries none.
func sessionDatabaseAt(t *testing.T, dsn string) (ompsessions.Database, *sessionDialer) {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := parsed.User.Password()
	t.Setenv("PGPASSWORD", password)
	address := parsed.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		address = net.JoinHostPort(address, "5432")
	}
	database := ompsessions.Database{Instance: testInstance, Name: strings.TrimPrefix(parsed.Path, "/"), User: parsed.User.Username()}
	return database, &sessionDialer{addr: address}
}

// Under a session database of the pod's own, the launcher's own resume lookup reaches the table
// through the generation's socket: a session the table holds resumes, and one it does not hold is
// refused with the same message as under any other SQL storage. The generation's environment points
// OMP_SESSION_SQL_DSN_FILE at the file the launcher wrote, as the pod manifest does, and that file
// holds the agent's spelling of the endpoint, which pgx does not read: a launcher that looked there
// would reach no database at all.
func TestAResumeUnderASessionDatabaseIsServedThroughTheProxy(t *testing.T) {
	dsn := testpg.Database(t, "legion_launcher_proxy_test")
	database, dialer := sessionDatabaseAt(t, dsn)
	shortenResumeLookup(t)
	g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	sessionFile := filepath.Join(t.TempDir(), "sessions", "2026-10-08T12-00-00-000Z_0004.jsonl")
	start := func(generation uint64) (string, bool) {
		command := g.childStart(generation, marker, nil)
		command.Env = append(command.Env, ompsessions.StorageVariable+"="+ompsessions.SQLStorage,
			ompsessions.DSNFileVariable+"="+filepath.Join(g.generationDir(generation), ompsessions.DSNFileName))
		command.ResumeFile = sessionFile
		got := g.start(command)
		return got.Error, got.OK
	}

	refusal, ok := start(3)
	if ok || !strings.Contains(refusal, "resume session "+sessionFile+": the session table holds no such session") {
		t.Fatalf("start of a session the table does not hold = %q (ok %t), want the refusal naming it", refusal, ok)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused resume started the child")
	}
	conn, err := ompsessions.ConnectURL(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := ompsessions.Import(context.Background(), conn, sessionFile, []byte("{}\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if refusal, ok := start(4); !ok {
		t.Fatalf("start of a session the table holds = %q, want generation 4 to run", refusal)
	}
	if seen := publishedAll(t, marker); seen.socket != "bound" {
		t.Fatalf("the child found its session socket %s, want it already bound", seen.socket)
	}
	g.stop(4)
}

// identityStub stands in for the pod's own cloud identity: a credential with an expiry, which the
// dialer presents at every dial and which only a fresh minting renews.
type identityStub struct {
	mu      sync.Mutex
	expires time.Time
}

func (s *identityStub) mint() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires = time.Now().Add(time.Minute)
}

func (s *identityStub) expire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires = time.Now().Add(-time.Second)
}

// present is the credential work a dial does before it reaches the database: an expired credential
// reaches nothing.
func (s *identityStub) present() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !time.Now().Before(s.expires) {
		return errors.New("the session database refused an expired credential")
	}
	return nil
}

// An agent goes on writing its session past the expiry of the credential its first connection used,
// with nothing restarting its generation: the proxy asks its dialer for a credential at every
// connection, so a credential minted afresh serves the next one. What this proves is only that: the
// proxy never pins the credential of its first dial. What it does not test is the connector's own
// refresh-ahead certificate cache, which is the library's behaviour and is tested there; the stub
// here mints on demand instead. The agent's own client is Bun's, and pgx through the same socket
// stands in for it, which is also the client the launcher itself looks a resume up with.
func TestAnAgentKeepsWritingPastItsCredentialsExpiry(t *testing.T) {
	database, dialer := sessionDatabaseAt(t, testpg.Database(t, "legion_launcher_expiry_test"))
	identity := &identityStub{}
	identity.mint()
	dialer.check = identity.present
	g := newSessionRig(t, Config{SessionDatabase: database, SessionDialer: dialer.build})
	marker := filepath.Join(t.TempDir(), "marker")
	if got := g.start(g.childStart(2, marker, nil)); !got.OK {
		t.Fatalf("start result = %#v", got)
	}
	child := publishedAll(t, marker)
	endpoint := ompsessions.Endpoint{SocketDir: g.generationDir(2), Database: database.Name, User: database.User}
	ask := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, err := lookupWritten(ctx, endpoint.ClientURL(), "sessions/one.jsonl")
		return err
	}

	if err := ask(); err != nil {
		t.Fatalf("the first connection through the proxy: %v", err)
	}
	identity.expire()
	if err := ask(); err == nil {
		t.Fatal("a connection reached the database on an expired credential")
	}
	identity.mint()
	if err := ask(); err != nil {
		t.Fatalf("a connection after the credential was minted afresh: %v", err)
	}
	if err := syscall.Kill(child.descendant, 0); err != nil {
		t.Fatalf("generation 2 did not survive the expiry: its descendant %d is gone (%v)", child.descendant, err)
	}
	g.stop(2)
}

// In an issue pod's one container the launcher and the role see a generation's directory at two
// paths, so the connection file the agent reads names the socket where the agent finds it: under
// the fixed root its own namespaces bind the role's private directory back at, and never the
// launcher's path under the roles directory, where the socket is really bound.
func TestAnIssuePodsSessionURLNamesTheSocketAsTheRoleSeesIt(t *testing.T) {
	fixed := t.TempDir()
	saved := roleFixedRoot
	roleFixedRoot = fixed
	t.Cleanup(func() { roleFixedRoot = saved })
	// The role's own view in a deployed pod, which the worker image makes the mount point for.
	if FixedRoot != "/var/run/legion" {
		t.Fatalf("the role's fixed root is %q, so a deployed role's view is not /var/run/legion", FixedRoot)
	}
	rolesDir := shortTempDir(t)
	role := "tester"
	for _, name := range []string{RoleLauncherDir, RolePrivateDir} {
		if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rolesDir, role, RoleLauncherDir, LauncherTokenFile), []byte("launcher-token\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	dialer := &sessionDialer{addr: idleUpstream(t)}
	managers, err := newManagers(Config{
		Connect: "tcp://127.0.0.1:1", Sandbox: "legion-legion-legion-208", RolesDir: rolesDir, Roles: []string{role},
		StopGrace: 2 * time.Second, SessionDatabase: testDatabase, SessionDialer: dialer.build,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := managers[0]
	if !m.namespaced {
		t.Fatal("the multi-role form built a manager that is not namespaced")
	}
	const generation = 4
	bound := generationDir(filepath.Join(rolesDir, role, RolePrivateDir), generation)
	if err := os.MkdirAll(bound, 0o700); err != nil {
		t.Fatal(err)
	}
	proxy, clientURL, err := m.startSession(context.Background(), generation, bound)
	if err != nil {
		t.Fatalf("startSession: %v; the launcher's log: %s", err, m.stderr)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	body, err := os.ReadFile(filepath.Join(bound, ompsessions.DSNFileName))
	if err != nil {
		t.Fatal(err)
	}
	seen := ompsessions.Endpoint{
		SocketDir: generationDir(filepath.Join(fixed, RolePrivateDir), generation),
		Database:  testDatabase.Name, User: testDatabase.User,
	}
	if string(body) != seen.AgentURL() {
		t.Fatalf("the connection file holds %q, want the role's own view %q", body, seen.AgentURL())
	}
	if strings.Contains(string(body), rolesDir) {
		t.Fatalf("the connection file names the launcher's own path under the roles directory: %q", body)
	}
	// The role's view in a deployed pod, which the test's fixed root stands in for.
	deployed := generationDir(FixedRoot+"/"+RolePrivateDir, generation)
	if deployed != "/var/run/legion/private/g4" {
		t.Fatalf("a deployed role reads its socket under %q, want /var/run/legion/private/g4", deployed)
	}

	// The socket itself is bound where the launcher is, under the role's directory of the roles
	// directory, and the URL the launcher's own client uses names that directory.
	socket := filepath.Join(bound, ompsessions.SocketName)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("the generation's socket is not bound under the role's own directory: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is %s, not a socket", socket, info.Mode())
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the generation's socket: %v", err)
	}
	_ = conn.Close()
	want := ompsessions.Endpoint{SocketDir: bound, Database: testDatabase.Name, User: testDatabase.User}.ClientURL()
	if clientURL != want {
		t.Fatalf("the launcher's own client URL is %q, want %q", clientURL, want)
	}
}
