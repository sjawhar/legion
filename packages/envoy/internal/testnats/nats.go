// Package testnats provides the NATS image and bounded readiness helpers for Envoy's NATS tests, and
// one server per test binary for the tests that need no server of their own.
//
// A package's tests share that server rather than start a container each: testcontainers keeps its
// Ryuk reaper only while a container of the process is connected, and a reaper left idle past its
// timeout removes itself, so a later start, finding it removing, fails ("unexpected container status
// removing"). Tests that stop, restart or configure their server still start their own (Start,
// StartRestartable); while the shared server runs, the reaper stays connected for them too.
//
// Each test gets the shared server as the user of a JetStream account no earlier test used (URL),
// so no test ever deletes and recreates a stream another test made. nats-server moves a deleted
// stream's directory aside and removes it from a background goroutine; a second delete of the same
// name before that goroutine has run leaves the stream's files where they were while it answers
// success, and the next create of that name recovers the deleted stream's messages. Every account
// keeps its streams in a directory of its own, so the names one test uses cannot reach another.
package testnats

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the NATS server image every Envoy test container runs. The envoy-go CI job reads this
// declaration and pulls the image before its first test step, so no test reaches the registry
// mid-run; keep it a single-line string constant.
const Image = "nats:2.10"

var (
	mainRuns   bool
	sharedOnce sync.Once
	shared     *accountServer
	sharedErr  error
)

// firstAccounts is how many accounts the shared server starts with. Every time a test asks for
// one past them, the server doubles them by a reload: a server's start time grows faster than its
// account count (0.9 s for 1,000 on a loaded devbox, 26 s for 5,000). The reload's wait,
// connectTimeout, caps that growth: at half a CPU, doubling 4,096 accounts took 36 s and 65 s in
// two measurements, so the test that asks for account 4,097 fails. A package run takes one account
// for each test and subtest that asks (103 for cmd/listener, 64 for internal/store), so -count=40
// of cmd/listener in one binary reaches the ceiling.
const firstAccounts = 256

// init runs in every test binary that imports this package, whichever helper its tests use. Every
// server these helpers start has no users unless a test configures some, and nats.go refuses an
// nkey when the server sends no nonce ("nats: nkeys not supported by the server"), so an
// operator's NATS_NKEY_SEED or NATS_NKEY_SEED_FILE never reaches the tests' clients. A test that
// means to pass a seed sets it itself.
func init() {
	os.Unsetenv("NATS_NKEY_SEED")
	os.Unsetenv("NATS_NKEY_SEED_FILE")
}

// Main runs the package's tests, then removes the shared server if a test started it, and returns
// the exit code. A package whose tests use URL calls it from TestMain: os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	if shared != nil {
		if err := testcontainers.TerminateContainer(shared.ctr); err != nil {
			fmt.Fprintf(os.Stderr, "remove the package's shared NATS container: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

// URL returns the package's shared NATS server as the user of a JetStream account no earlier test
// used, so none of a previous test's streams, KV buckets, consumers or messages is there, and no
// stream name another test deleted can be recovered under it. A test gets one account however
// often it asks, so every connection it makes reaches the same streams; each subtest is a test of
// its own and gets its own account, since subtests that each delete and recreate one name in a
// shared account would race as tests did on a shared server.
// When the test ends, its account's streams are deleted to free their storage; nothing creates a
// stream in that account again, so a delete that leaves files behind reaches no test.
func URL(t testing.TB) string {
	t.Helper()
	if !mainRuns {
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which removes the shared NATS container")
	}
	sharedOnce.Do(func() { shared, sharedErr = startAccountServer(firstAccounts) })
	if sharedErr != nil {
		t.Fatalf("start the shared NATS: %v", sharedErr)
	}
	return shared.url(t)
}

// accountServer is a NATS server that hands each test a JetStream account no earlier test used.
type accountServer struct {
	ctr  *tcnats.NATSContainer
	host string

	mu sync.Mutex
	// declared is how many accounts the server's configuration declares, and handedOut how many of
	// them it has handed to a test. handedOut only grows: an account is never handed out twice.
	declared  int
	handedOut int
	// holders maps each test holding an account to that account's URL.
	holders map[string]string
}

// startAccountServer starts a server declaring accounts accounts. Its log carries no -DV protocol
// trace, since a reload's outcome is read from the log.
func startAccountServer(accounts int) (*accountServer, error) {
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, Image,
		testcontainers.WithCmd("-js"),
		tcnats.WithConfigFile(strings.NewReader(accountsConfig(accounts))))
	if err != nil {
		return nil, errors.Join(err, testcontainers.TerminateContainer(ctr))
	}
	host, err := ctr.PortEndpoint(ctx, "4222/tcp", "")
	if err != nil {
		return nil, errors.Join(err, testcontainers.TerminateContainer(ctr))
	}
	return &accountServer{ctr: ctr, host: host, declared: accounts, holders: map[string]string{}}, nil
}

// url is URL on s.
func (s *accountServer) url(t testing.TB) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if uri, ok := s.holders[t.Name()]; ok {
		return uri
	}
	if s.handedOut == s.declared {
		if outcome := reload(t, s.ctr, accountsConfig(2*s.declared)); outcome != reloaded {
			t.Fatalf("NATS refused %d accounts: %s", 2*s.declared, outcome)
		}
		s.declared *= 2
	}
	s.handedOut++
	account := s.handedOut
	uri := fmt.Sprintf("nats://t%d:t%d@%s", account, account, s.host)
	// Wait for the account to answer, after the start or the reload that declared it.
	Connect(t, uri).Close()
	name := t.Name()
	s.holders[name] = uri
	t.Cleanup(func() {
		s.mu.Lock()
		delete(s.holders, name)
		s.mu.Unlock()
		deleteStreams(t, uri)
	})
	return uri
}

// accountsConfig declares accounts T1 to Tn, each with JetStream and one user, tn, whose password
// is its name.
func accountsConfig(n int) string {
	var config strings.Builder
	config.WriteString("accounts {\n")
	for account := 1; account <= n; account++ {
		fmt.Fprintf(&config, "  T%d: { jetstream: enabled, users: [ { user: t%d, password: t%d } ] }\n", account, account, account)
	}
	config.WriteString("}\n")
	return config.String()
}

// deleteStreams deletes every stream in the account uri names, as its test ends.
func deleteStreams(t testing.TB, uri string) {
	t.Helper()
	conn := Connect(t, uri)
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Errorf("open JetStream to delete the test's streams: %v", err)
		return
	}
	var streams []string
	for name := range js.StreamNames() {
		streams = append(streams, name)
	}
	for _, name := range streams {
		if err := js.DeleteStream(name); err != nil && !errors.Is(err, natsgo.ErrStreamNotFound) {
			t.Errorf("delete the test's stream %s: %v", name, err)
		}
	}
}

// Start runs a NATS test container on Image, removed when the test ends (even when its start
// fails), and returns it with its client URL.
func Start(t testing.TB) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, Image)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	return ctr, uri
}

// StartNkeyAuthorized runs a NATS test container on Image, with JetStream, that accepts only
// clients authenticating as the nkey user whose public key is user, removed when the test ends,
// and returns its client URL once the server answers there (answering).
func StartNkeyAuthorized(t testing.TB, user string) string {
	t.Helper()
	_, uri := startNkeyConfig(t, nkeyConfig(fmt.Sprintf("{ nkey: %q }", user)))
	return uri
}

// NkeyGrant is a NATS test server, started by StartNkeyGranted, whose one nkey user may publish
// only to the subjects its grant names; Grant changes the grant.
type NkeyGrant struct {
	URL  string
	user string
	ctr  *tcnats.NATSContainer
}

// StartNkeyGranted runs a NATS test container as StartNkeyAuthorized does, whose one user may
// publish only to the subjects allow names (and subscribe to anything): a publish to any other
// subject is the server's permissions violation, as a per-client grant makes it. The test can
// change the grant.
func StartNkeyGranted(t testing.TB, user string, allow ...string) *NkeyGrant {
	t.Helper()
	ctr, uri := startNkeyConfig(t, publishGrantConfig(user, allow))
	return &NkeyGrant{URL: uri, user: user, ctr: ctr}
}

// Grant replaces the user's publish grant with allow and has the server reload its configuration,
// as an operator changing a grant does: the server keeps its connections and applies the new grant
// to them. It fails the test, with the server's reason, when the server refused the new
// configuration.
func (g *NkeyGrant) Grant(t testing.TB, allow ...string) {
	t.Helper()
	if outcome := reload(t, g.ctr, publishGrantConfig(g.user, allow)); outcome != reloaded {
		t.Fatalf("NATS refused the new grant %q: %s", allow, outcome)
	}
}

// reloaded is the reload outcome nats-server logs when it took the new configuration.
const reloaded = "Reloaded server configuration"

// reloadOutcome matches each line nats-server logs when a reload ends: done, or refused with its
// reason.
var reloadOutcome = regexp.MustCompile(reloaded + `|Failed to reload server configuration: [^\r\n]*`)

// reload writes config as ctr's configuration file and signals the server to reload it, as an
// operator does: the server keeps its connections. It returns this reload's outcome (reloaded, or
// the server's refusal) once the server has logged it, not an earlier one's: it waits for one more
// outcome line than the server's log held before the signal.
func reload(t testing.TB, ctr *tcnats.NATSContainer, config string) string {
	t.Helper()
	ctx := context.Background()
	earlier := len(reloadOutcomes(t, ctr))
	if err := ctr.CopyToContainer(ctx, []byte(config), "/etc/nats.conf", 0o644); err != nil {
		t.Fatalf("write the new configuration: %v", err)
	}
	docker, err := testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	defer docker.Close()
	if err := docker.ContainerKill(ctx, ctr.GetContainerID(), "HUP"); err != nil {
		t.Fatalf("signal NATS to reload: %v", err)
	}
	done := wait.ForLog(reloadOutcome.String()).AsRegexp().WithOccurrence(earlier + 1).WithStartupTimeout(connectTimeout)
	if err := done.WaitUntilReady(ctx, ctr); err != nil {
		t.Fatalf("NATS did not report reloading its configuration: %v", err)
	}
	// This reload's outcome is the one after the earlier ones, whatever may have followed it.
	return reloadOutcomes(t, ctr)[earlier]
}

// reloadOutcomes lists the reload outcomes ctr's log reports so far, oldest first.
func reloadOutcomes(t testing.TB, ctr *tcnats.NATSContainer) []string {
	t.Helper()
	logs, err := ctr.Logs(context.Background())
	if err != nil {
		t.Fatalf("read NATS's log: %v", err)
	}
	defer logs.Close()
	text, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read NATS's log: %v", err)
	}
	return reloadOutcome.FindAllString(string(text), -1)
}

func publishGrantConfig(user string, allow []string) string {
	quoted := make([]string, len(allow))
	for index, subject := range allow {
		quoted[index] = fmt.Sprintf("%q", subject)
	}
	return nkeyConfig(fmt.Sprintf("{ nkey: %q, permissions: { publish: { allow: [%s] } } }",
		user, strings.Join(quoted, ", ")))
}

func nkeyConfig(entry string) string {
	return fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ %s ]\n}\n", entry)
}

func startNkeyConfig(t testing.TB, config string) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcnats.Run(ctx, Image, tcnats.WithConfigFile(strings.NewReader(config)))
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start nkey-authorized NATS: %v", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("NATS connection string: %v", err)
	}
	answering(t, uri)
	return ctr, uri
}

// answering waits, within connectTimeout, for the server at uri to refuse a connection with no
// credential with its own authorization violation: the proof it speaks the client protocol and
// enforces its nkey users. Testcontainers can report a mapped port before the NATS protocol
// handshake, and a dial then ends in EOF or a refusal that says nothing about the server's users.
// A server that admits the connection enforces no users, and every refusal a test expects of it
// would pass for the wrong reason, so that fails the test.
func answering(t testing.TB, uri string) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	var err error
	for time.Now().Before(deadline) {
		var conn *natsgo.Conn
		if conn, err = natsgo.Connect(uri, natsgo.Timeout(dialTimeout), natsgo.NoReconnect()); err == nil {
			conn.Close()
			t.Fatalf("NATS at %s admitted a client with no credential: it enforces no nkey users", uri)
		}
		if errors.Is(err, natsgo.ErrAuthorization) {
			return
		}
		time.Sleep(retryInterval)
	}
	t.Fatalf("NATS at %s did not answer within %s: %v", uri, connectTimeout, err)
}

// StartRestartable runs a NATS test container a test can stop and start as a server restarts, and
// returns it, once it answers, with a URL that stays the server's across every restart. The URL's
// port is a listener this process holds for the test's life, which relays each connection to
// wherever Docker maps the container's client port at that moment. Docker releases a stopped
// container's host port, so a fixed mapping could be taken by another container or a free-port
// pick before the restart, and the restart then fails with "address already in use". A
// connection made while the server is down is closed at once, as a refused dial would be, and one
// the server drops is dropped on the client's side too. A restart keeps the container's JetStream
// store, as a server restarted on its own volume does.
func StartRestartable(t testing.TB) (*tcnats.NATSContainer, string) {
	t.Helper()
	ctr, err := tcnats.Run(context.Background(), Image)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start NATS: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold the server's port: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go relay(listener, func(ctx context.Context) (string, error) {
		return ctr.PortEndpoint(ctx, "4222/tcp", "")
	})
	uri := "nats://" + listener.Addr().String()
	Connect(t, uri).Close()
	return ctr, uri
}

// relay joins each connection listener accepts to the server's current address, until the
// listener closes.
func relay(listener net.Listener, server func(context.Context) (string, error)) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		go join(client, server)
	}
}

// join copies client and the server's connection into each other until either side ends, then
// closes both. A server without a mapped port (it is stopped) or one that refuses the dial ends
// the client at once.
func join(client net.Conn, server func(context.Context) (string, error)) {
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	address, err := server(ctx)
	cancel()
	if err != nil {
		return
	}
	upstream, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		return
	}
	defer upstream.Close()
	ended := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); ended <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); ended <- struct{}{} }()
	<-ended
}

// Stop stops a NATS test container within one second, which its Start restarts.
func Stop(t testing.TB, ctr *tcnats.NATSContainer) {
	t.Helper()
	timeout := time.Second
	if err := ctr.Stop(context.Background(), &timeout); err != nil {
		t.Fatalf("stop NATS: %v", err)
	}
}

const (
	connectTimeout = 30 * time.Second
	retryInterval  = 100 * time.Millisecond
	dialTimeout    = time.Second
)

// Connect waits for a test NATS server to accept a real connection and JetStream to answer an
// operation before returning it. Testcontainers can report a mapped port before the NATS protocol
// handshake, and NATS can complete that handshake while JetStream is still starting; a KV test
// needs both readiness boundaries.
func Connect(t testing.TB, uri string) *natsgo.Conn {
	t.Helper()

	deadline := time.Now().Add(connectTimeout)
	var conn *natsgo.Conn
	var err error
	for time.Now().Before(deadline) {
		conn, err = natsgo.Connect(uri, natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
		if err == nil {
			break
		}
		time.Sleep(retryInterval)
	}
	if conn == nil {
		t.Fatalf("failed to connect to NATS within %s: %v", connectTimeout, err)
		return nil
	}
	waitForJetStream(t, func() error {
		js, err := conn.JetStream()
		if err != nil {
			return err
		}
		_, err = js.AccountInfo()
		return err
	})
	return conn
}

// waitForJetStream retries a real JetStream operation rather than trusting a container port or a
// successful core-NATS connect. Kept separate so its transient boundary has a direct unit test.
func waitForJetStream(t testing.TB, check func() error) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(retryInterval)
	}
	t.Fatalf("JetStream was not ready within %s: %v", connectTimeout, err)
}
