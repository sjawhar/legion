// Package testnats runs NATS servers for Envoy's tests in the test process itself, and gives the
// tests that need no server of their own one server per test binary.
//
// Every server these helpers start is the nats-server this binary links in
// (github.com/nats-io/nats-server/v2), not a container: a test that needed a container runtime
// could not run where Envoy's agents do, which have none. Each server listens on a loopback port
// of its own choosing and is reached through a relay this process owns. The relay's port is the
// one the test's URL names and it holds that port for the test's life, which buys three things: a
// URL that survives a stop and a start (Server.Stop, Server.Start), a way to stop the bytes
// moving in either direction without dropping a connection (Server.Freeze), and a port no other
// test binary can take while the test runs.
//
// A package's tests share one server rather than start one each, and each test gets it as the user
// of a JetStream account no earlier test used (URL), so no test ever deletes and recreates a
// stream another test made. nats-server moves a deleted stream's directory aside and removes it
// from a background goroutine; a second delete of the same name before that goroutine has run
// leaves the stream's files where they were while it answers success, and the next create of that
// name recovers the deleted stream's messages. Every account keeps its streams in a directory of
// its own, so the names one test uses cannot reach another.
package testnats

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// Image is the NATS server image every Envoy test container runs. The envoy-go CI job reads this
// declaration and pulls the image before its first test step, so no test reaches the registry
// mid-run; keep it a single-line string constant.
const Image = "mirror.gcr.io/library/nats:2.10"

// HubImage is the NATS server image the GCP hub runs, pinned by digest. The envoy-go and
// envoy-infra-go CI jobs read this declaration and pull the image before their tests run; keep it
// a single-line string constant.
const HubImage = "mirror.gcr.io/library/nats:2.15.1-alpine@sha256:2ae57dfba3063d5e25fecb75256118c0882a1a2d2cf7d0fba17a7c701df956f6"

// Images are the two NATS server images the real-server tests run on, Image and HubImage, each
// with the short name its subtest takes.
var Images = []struct{ Name, Ref string }{{"2.10", Image}, {"hub", HubImage}}

var (
	mainRuns      bool
	sharedOnce    sync.Once
	shared        *accountServer
	sharedCleanup *sharedTB
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

// Main runs the package's tests, then shuts the shared server down and removes its store if a test
// started it, and returns the exit code. A package whose tests use URL calls it from TestMain:
// os.Exit(testnats.Main(m)).
func Main(m *testing.M) int {
	mainRuns = true
	code := m.Run()
	if sharedCleanup != nil {
		sharedCleanup.run()
		if sharedCleanup.failed && code == 0 {
			code = 1
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
		t.Fatal("testnats: the package's TestMain must return testnats.Main(m), which shuts the shared NATS server down")
	}
	sharedOnce.Do(func() {
		sharedCleanup = &sharedTB{TB: t}
		shared = startAccountServer(sharedCleanup, firstAccounts)
	})
	if shared == nil {
		t.Fatal("testnats: the package's shared NATS server did not start; the test that first asked for it reports why")
	}
	return shared.url(t)
}

// sharedTB stands in for the test that first asks for the package's shared server. That server
// outlives the test, so the cleanup start registers on it runs from Main, after the last test, and
// what the cleanup reports goes to stderr and the exit code: a t.Errorf on a test that has already
// ended panics.
type sharedTB struct {
	testing.TB
	cleanups []func()
	failed   bool
}

func (s *sharedTB) Cleanup(clean func()) { s.cleanups = append(s.cleanups, clean) }

func (s *sharedTB) Errorf(format string, args ...any) {
	s.failed = true
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// run runs the cleanups the shared server registered, the most recent first, as testing does.
func (s *sharedTB) run() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
}

// accountServer is a NATS server that hands each test a JetStream account no earlier test used.
type accountServer struct {
	srv  *Server
	host string

	mu sync.Mutex
	// declared is how many accounts the server's configuration declares, and handedOut how many of
	// them it has handed to a test. handedOut only grows: an account is never handed out twice.
	declared  int
	handedOut int
	// holders maps each test holding an account to that account's URL.
	holders map[string]string
}

// startAccountServer starts a server declaring accounts accounts, removed when t ends.
func startAccountServer(t testing.TB, accounts int) *accountServer {
	t.Helper()
	srv := start(t, accountsConfig(accounts), []string{"-js"})
	return &accountServer{
		srv:      srv,
		host:     strings.TrimPrefix(srv.URL(), "nats://"),
		declared: accounts,
		holders:  map[string]string{},
	}
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
		if err := s.srv.Reload(t, accountsConfig(2*s.declared)); err != nil {
			t.Fatalf("NATS refused %d accounts: %v", 2*s.declared, err)
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

// Server is a nats-server running in this test process, reached through a relay this process owns.
// Every helper of this package that starts a server returns one, and start is the only thing that
// makes one.
type Server struct {
	// store is the server's JetStream store, configPath its configuration file and args the
	// arguments it was started with: a restart and a reload read the configuration again from that
	// file, so a Reload the server took is the configuration a later start serves.
	store      string
	configPath string
	args       []string
	log        *logCapture
	relay      *relay
	url        string
	// name is the server_name StartCluster gives a member, "" for a lone server.
	name string

	mu        sync.Mutex
	srv       *natsserver.Server
	jetStream bool
	monitor   string
}

// URL is the server's client URL, which stays the same across every Stop and Start: it names the
// relay's port, not the server's own.
func (s *Server) URL() string { return s.url }

// Name is the server_name of a cluster member (StartCluster), the name JetStream reports it by in
// a stream's or a consumer's cluster information. A lone server has none and answers "".
func (s *Server) Name() string { return s.name }

// MonitorURL is the server's monitoring endpoint, where /varz and /connz are. The monitoring
// listener is the server's own rather than the relay's, so a Stop and a Start give it a new port
// and a test reads this again after one.
func (s *Server) MonitorURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.monitor
}

// Stop shuts the server down, as stopping a server an operator restarts does, and keeps its store
// and its URL for the Start that follows. Stopping a stopped server does nothing.
func (s *Server) Stop(t testing.TB) {
	t.Helper()
	s.stop()
}

func (s *Server) stop() {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.monitor = ""
	s.mu.Unlock()
	if srv == nil {
		return
	}
	srv.Shutdown()
	srv.WaitForShutdown()
}

// Start runs the server again on the store and the configuration it had, and returns once it
// accepts connections. Its clients reach it at the same URL: the relay held the port while it was
// down, and now joins each connection to wherever the server listens.
func (s *Server) Start(t testing.TB) {
	t.Helper()
	s.mu.Lock()
	running := s.srv != nil
	s.mu.Unlock()
	if running {
		t.Fatal("start a NATS server that is already running")
	}
	s.launch(t)
}

// Terminate shuts the server down for good and closes its relay, which frees the URL's port. The
// cleanup start registers does this itself, so a test calls it only to end a server early.
func (s *Server) Terminate(t testing.TB) {
	t.Helper()
	s.stop()
	s.relay.Close()
}

// Freeze stops the relay moving bytes between the test and the server, in either direction, and
// returns the func that thaws it. Both sides keep their connections, so a request made while the
// relay is frozen times out on a connection that is still CONNECTED, as a server that has stopped
// answering leaves it, and the traffic resumes when the returned func runs.
func (s *Server) Freeze(t testing.TB) func() {
	t.Helper()
	return s.relay.freeze()
}

// Reload has the running server take config as its whole configuration, as an operator signalling
// a reload does: the server keeps its connections and applies the new configuration to them. It
// returns the server's own refusal when the server would not take the configuration, and nil when
// it did. A refused configuration never becomes the server's: the file a restart reads keeps the
// configuration the server is serving.
func (s *Server) Reload(t testing.TB, config string) error {
	t.Helper()
	s.mu.Lock()
	srv, jetStream := s.srv, s.jetStream
	s.mu.Unlock()
	if srv == nil {
		t.Fatal("reload the configuration of a NATS server that is not running")
	}
	pending := s.configPath + ".pending"
	if err := os.WriteFile(pending, []byte(config), 0o600); err != nil {
		t.Fatalf("write the new configuration: %v", err)
	}
	defer func() { _ = os.Remove(pending) }()
	opts, err := serverOptions(pending, s.args, s.store)
	if err != nil {
		return err
	}
	// ReloadOptions refuses an Options whose listening ports differ from the ones the server was
	// started with ("config reload not supported for HTTPPort: old=-1, new=52544"), so the new
	// options carry what the server was started with, which serverOptions imposes, and never a
	// port read back from the running server. The configuration file and JetStream are the
	// server's as well: a reload can neither move the one nor turn the other on or off.
	opts.ConfigFile = s.configPath
	opts.JetStream = jetStream
	// ConfigureOptions and ReloadOptions meet over natsserver.FlagSnapshot, a package-level
	// variable of nats-server; serverOptions clears it under the same lock this takes.
	flagsMu.Lock()
	err = srv.ReloadOptions(opts)
	flagsMu.Unlock()
	if err != nil {
		return err
	}
	if err := os.Rename(pending, s.configPath); err != nil {
		t.Fatalf("keep the configuration the server took: %v", err)
	}
	return nil
}

// Restart stops the server, makes config its whole configuration and starts it again on the same
// store and URL, as an operator restarts a server after changing a grant, and returns once it
// accepts connections. Unlike Reload, every connection drops: each client reconnects to the server
// config governs and sends its subscriptions again, which that server accepts or refuses afresh.
func (s *Server) Restart(t testing.TB, config string) {
	t.Helper()
	s.stop()
	if err := os.WriteFile(s.configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write the new configuration: %v", err)
	}
	s.launch(t)
}

// Log is everything the server has logged so far, the debug and protocol trace among it when the
// server was started with -DV.
func (s *Server) Log() string { return s.log.String() }

// address is where the server listens for clients at this moment, which the relay dials. A stopped
// server has none, and the relay ends the connection as a refused dial would.
func (s *Server) address() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return "", errors.New("the NATS server is stopped")
	}
	addr := s.srv.Addr()
	if addr == nil {
		return "", errors.New("the NATS server has no client address")
	}
	return addr.String(), nil
}

// clusterAddress is where the server listens for routes at this moment, which its route relay
// dials (StartCluster). A stopped server has none, as address says.
func (s *Server) clusterAddress() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return "", errors.New("the NATS server is stopped")
	}
	addr := s.srv.ClusterAddr()
	if addr == nil {
		return "", errors.New("the NATS server has no route address")
	}
	return addr.String(), nil
}

// natsServer is the running nats-server, or nil while the server is stopped.
func (s *Server) natsServer() *natsserver.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srv
}

// running reports whether the server is running: started and not stopped since.
func (s *Server) running() bool { return s.natsServer() != nil }

// hostOnlyConfig is the configuration of a server a test starts with none of its own:
// nats-server warns on an empty configuration file ("config has no values or is empty"), and the
// host it names here is the one this rig imposes on every server anyway. The file is never empty,
// so a server started without a configuration can still be reloaded with one.
const hostOnlyConfig = "host: 127.0.0.1\n"

// start runs a nats-server in this process whose whole configuration is config, with serverArgs as
// its command line after it (so -js for JetStream, -DV for the protocol trace), behind a relay
// holding the port of the URL it returns, and returns it once it accepts connections. When the
// test ends the server is shut down, the relay closed and the server's directory removed. It is
// the one place a server of this package is made: every helper here is a call on it.
func start(t testing.TB, config string, serverArgs []string) *Server {
	t.Helper()
	if config == "" {
		config = hostOnlyConfig
	}
	dir, err := os.MkdirTemp("", "testnats")
	if err != nil {
		t.Fatalf("make the server's directory: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("hold the server's port: %v", err)
	}
	server := &Server{
		store:      filepath.Join(dir, "store"),
		configPath: filepath.Join(dir, "nats.conf"),
		args:       serverArgs,
		log:        &logCapture{},
		url:        "nats://" + listener.Addr().String(),
	}
	server.relay = newRelay(listener, server.address)
	t.Cleanup(func() {
		server.stop()
		server.relay.Close()
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove the test NATS server's directory: %v", err)
		}
	})
	if err := os.WriteFile(server.configPath, []byte(config), 0o600); err != nil {
		t.Fatalf("write the server's configuration: %v", err)
	}
	server.launch(t)
	return server
}

// launch runs a nats-server on the server's configuration and store, and returns once it accepts
// connections.
func (s *Server) launch(t testing.TB) {
	t.Helper()
	opts, err := serverOptions(s.configPath, s.args, s.store)
	if err != nil {
		t.Fatalf("read the NATS configuration: %v", err)
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create the NATS server: %v", err)
	}
	srv.SetLoggerV2(s.log, opts.Debug, opts.Trace, false)
	go srv.Start()
	if !srv.ReadyForConnections(connectTimeout) {
		srv.Shutdown()
		srv.WaitForShutdown()
		t.Fatalf("NATS did not accept connections within %s: %s", connectTimeout, s.log.String())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.srv = srv
	s.jetStream = opts.JetStream
	if addr := srv.MonitorAddr(); addr != nil {
		s.monitor = "http://" + addr.String()
	}
}

// flagsMu serialises the calls that meet over natsserver.FlagSnapshot: ConfigureOptions sets it and
// ReloadOptions merges it over the options it is given, whichever server made it.
var flagsMu sync.Mutex

// serverOptions parses the configuration at path and serverArgs as nats-server parses its own
// command line, then imposes what this rig owns: loopback, a client and a monitoring port the
// server picks, the test's own store, no signal handlers, and path as the configuration file.
// serverArgs replace nats-server's defaults rather than add to them, as the arguments after the
// image's own command do.
func serverOptions(path string, serverArgs []string, store string) (*natsserver.Options, error) {
	flags := flag.NewFlagSet("nats-server", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	silent := func() {}
	flagsMu.Lock()
	opts, err := natsserver.ConfigureOptions(flags, append([]string{"-c", path}, serverArgs...), silent, silent, silent)
	// ConfigureOptions leaves its parse in natsserver.FlagSnapshot, which every later ReloadOptions
	// in this process merges over the configuration it is given, whatever server it belongs to.
	// Each server here carries its own options, so nothing of this parse outlives it.
	natsserver.FlagSnapshot = nil
	flagsMu.Unlock()
	if err != nil {
		return nil, err
	}
	opts.Host = "127.0.0.1"
	opts.Port = -1
	opts.HTTPHost = "127.0.0.1"
	opts.HTTPPort = -1
	opts.StoreDir = store
	opts.NoSigs = true
	opts.ConfigFile = path
	return opts, nil
}

// logCapture is the server's logger: it keeps every line the server logs, for Server.Log.
type logCapture struct {
	mu   sync.Mutex
	text strings.Builder
}

func (c *logCapture) write(level, format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.text, "[%s] %s\n", level, line)
}

func (c *logCapture) Noticef(format string, args ...any) { c.write("INF", format, args...) }
func (c *logCapture) Warnf(format string, args ...any)   { c.write("WRN", format, args...) }
func (c *logCapture) Errorf(format string, args ...any)  { c.write("ERR", format, args...) }
func (c *logCapture) Fatalf(format string, args ...any)  { c.write("FTL", format, args...) }
func (c *logCapture) Debugf(format string, args ...any)  { c.write("DBG", format, args...) }
func (c *logCapture) Tracef(format string, args ...any)  { c.write("TRC", format, args...) }

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text.String()
}

// relay holds the port a server's URL names for the test's life and joins each connection it
// accepts to wherever that server listens at the moment the connection arrives. The server's own
// port is one it picks at each start, which a restart therefore changes and the relay hides; a
// connection made while the server is down is closed at once, as a refused dial would be, and one
// the server drops is dropped on the client's side too.
type relay struct {
	listener net.Listener
	target   func() (string, error)
	done     chan struct{}
	closing  sync.Once

	mu sync.Mutex
	// gate is nil while the relay moves bytes, and open while it is frozen: every copy waits for
	// it to close, which thawing does.
	gate chan struct{}
}

func newRelay(listener net.Listener, target func() (string, error)) *relay {
	r := &relay{listener: listener, target: target, done: make(chan struct{})}
	go r.serve()
	return r
}

// serve joins each connection the relay accepts to the server, until the relay closes.
func (r *relay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.join(client)
	}
}

// join copies client and the server's connection into each other until either side ends, then
// closes both. One direction runs on this goroutine and the other on one of its own, and whichever
// ends first closes both connections, which ends the other.
func (r *relay) join(client net.Conn) {
	address, err := r.target()
	if err != nil {
		_ = client.Close()
		return
	}
	upstream, err := net.DialTimeout("tcp", address, dialTimeout)
	if err != nil {
		_ = client.Close()
		return
	}
	ended := sync.OnceFunc(func() {
		_ = upstream.Close()
		_ = client.Close()
	})
	go func() {
		defer ended()
		r.stream(upstream, client)
	}()
	defer ended()
	r.stream(client, upstream)
}

// stream copies src into dst, a frozen relay moving nothing: the read and the write each wait for
// the thaw, so the bytes stay where they were and both connections stay open.
func (r *relay) stream(dst io.Writer, src io.Reader) {
	buffer := make([]byte, 32*1024)
	for {
		r.pass()
		read, err := src.Read(buffer)
		if read > 0 {
			r.pass()
			if _, err := dst.Write(buffer[:read]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pass waits while the relay is frozen, and returns at once once it is thawed or closed.
func (r *relay) pass() {
	r.mu.Lock()
	gate := r.gate
	r.mu.Unlock()
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-r.done:
	}
}

// freeze stops the relay moving bytes and returns the func that starts it again, which runs once
// however often it is called.
func (r *relay) freeze() func() {
	r.mu.Lock()
	if r.gate == nil {
		r.gate = make(chan struct{})
	}
	gate := r.gate
	r.mu.Unlock()
	return sync.OnceFunc(func() {
		r.mu.Lock()
		owned := r.gate == gate
		if owned {
			r.gate = nil
		}
		r.mu.Unlock()
		// A second freeze while this one held the gate shares it: only the thaw that still owns
		// the gate closes it, so the other returns rather than closing it twice.
		if !owned {
			return
		}
		close(gate)
	})
}

// Close frees the port the relay held and releases every copy a freeze holds, which then runs on
// as an unfrozen copy does: a copy keeps running until one of its endpoints ends.
func (r *relay) Close() {
	r.closing.Do(func() {
		close(r.done)
		_ = r.listener.Close()
	})
}

// Start runs a NATS server with JetStream, shut down when the test ends (even when its start
// fails), and returns it with its client URL. That URL stays the server's across every Stop and
// Start a test makes: it names a port this process holds for the test's life, whatever port the
// server itself listens on after a restart, and the restart keeps the server's JetStream store,
// as a server restarted on its own volume does.
func Start(t testing.TB) (*Server, string) {
	t.Helper()
	server := start(t, "", []string{"-js"})
	return server, server.URL()
}

// StartNkeyAuthorized runs a NATS server with JetStream that accepts only clients authenticating
// as the nkey user whose public key is user, shut down when the test ends, and returns its client
// URL once the server refuses a client with no credential (awaitAuth).
func StartNkeyAuthorized(t testing.TB, user string) string {
	t.Helper()
	_, uri := StartNkeyAuthorizedServer(t, user)
	return uri
}

// StartNkeyAuthorizedServer runs the server StartNkeyAuthorized runs and returns it with its
// client URL, for a test that must stop and start it under a connected client.
func StartNkeyAuthorizedServer(t testing.TB, user string) (*Server, string) {
	t.Helper()
	return startNkeyConfig(t, nkeyConfig(fmt.Sprintf("{ nkey: %q }", user)))
}

// Gate is one TCP address a test holds for its whole life, which relays to srv while it is open
// and has nothing listening at all while it is closed.
//
// nats.go runs ReconnectErrCB only for a reconnect attempt whose dial failed: an attempt that
// connects and is then dropped is silent. The server's own relay accepts every connection while
// the server is stopped and closes it, so a test that needs a connected client's reconnect
// attempts to fail needs an address nothing listens on, and a test that then needs a successful
// reconnect needs that same address to come back once the server is up. The gate's open relay
// dials the server's URL, which names the server's relay and so survives Server.Stop and
// Server.Start.
type Gate struct {
	t       testing.TB
	address string
	server  string

	mu sync.Mutex
	// relay is the gate's listener and its copies while the gate is open, and nil while it is
	// closed.
	relay *relay
}

// A gate's port is one no :0 bind can be handed. While the gate is closed nothing listens at its
// address, and it cannot hold the port instead: nats.go runs ReconnectErrCB only for a reconnect
// whose dial failed, so a held or accepting port would let the test the gate serves pass while
// testing nothing. Its port is therefore free whenever it is closed, and every :0 bind on the
// machine - each relay of this rig, each server's client and monitoring listener at every start,
// another test binary's - takes its port from the kernel's ephemeral range, so a gate port inside
// that range can be taken while the gate is closed, and Open then fails or the client's dials
// reach someone else's listener.
//
// That range is the machine's, not a constant: Linux's default is 32768-60999, and in a Legion
// worker pod (gVisor) it is 16000-65535. So the gate reads it from ephemeralRangePath and picks
// from the gatePortCount ports just below its first port (gatePorts).
const (
	ephemeralRangePath = "/proc/sys/net/ipv4/ip_local_port_range"
	// fallbackEphemeralLow is the first ephemeral port assumed when ephemeralRangePath cannot be
	// read or parsed: the lowest any machine this rig runs on has shown, so an unknown machine
	// gets the band that the most ranges leave alone rather than Linux's default.
	fallbackEphemeralLow = 16000
	gatePortCount        = 1000
	// gatePortFloor is the lowest port a gate's band may start at: below it lie the ports that
	// system services and fixed listeners take.
	gatePortFloor = 2048
)

// gatePorts returns the band a gate picks its port from, first through first+count-1, given text,
// the content of ephemeralRangePath: two decimal fields, the range's first port and its last. Text
// that is not such a range is read as a range starting at fallbackEphemeralLow. It returns an error
// naming the range when the band below its first port would start under gatePortFloor.
func gatePorts(text string) (first, count int, err error) {
	low := fallbackEphemeralLow
	if fields := strings.Fields(text); len(fields) == 2 {
		parsedLow, lowErr := strconv.Atoi(fields[0])
		parsedHigh, highErr := strconv.Atoi(fields[1])
		if lowErr == nil && highErr == nil && 0 < parsedLow && parsedLow <= parsedHigh && parsedHigh <= 65535 {
			low = parsedLow
		}
	}
	first = low - gatePortCount
	if first < gatePortFloor {
		return 0, 0, fmt.Errorf("the ephemeral range %q starts at port %d, so the %d ports below it would start at %d, under %d: no band there is free of both the kernel's own ports and the ones fixed services listen on", strings.TrimSpace(text), low, gatePortCount, first, gatePortFloor)
	}
	return first, gatePortCount, nil
}

// StartGate opens a gate on a free port of the band gatePorts chooses from this machine's
// ephemeral range, relaying to srv, and closes it when the test ends.
func StartGate(t testing.TB, srv *Server) *Gate {
	t.Helper()
	// An unreadable file is empty text, which gatePorts reads as the fallback range.
	text, _ := os.ReadFile(ephemeralRangePath)
	first, count, err := gatePorts(string(text))
	if err != nil {
		t.Fatalf("choose a gate port: %v", err)
		return nil
	}
	g := &Gate{t: t, server: strings.TrimPrefix(srv.URL(), "nats://")}
	var last error
	for range 100 {
		address := fmt.Sprintf("127.0.0.1:%d", first+rand.IntN(count))
		listener, err := net.Listen("tcp", address)
		if err != nil {
			last = err
			continue
		}
		g.address = address
		g.relay = newRelay(listener, g.target)
		t.Cleanup(g.Close)
		return g
	}
	t.Fatalf("hold a gate port: %v", last)
	return nil
}

// target is the address the gate's relay dials: the server's relay, whose port the server's URL
// names for the test's life.
func (g *Gate) target() (string, error) { return g.server, nil }

// URL is the client URL of the gate's address, which is the same for the test's whole life.
func (g *Gate) URL() string { return "nats://" + g.address }

// Close stops listening, so every dial of the gate's address is refused. It does nothing when the
// gate is already closed. It does not break a connection already joined through the gate, which
// keeps copying until one side ends, so a test that needs a connected client to lose its
// connection stops the server too.
func (g *Gate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relay == nil {
		return
	}
	g.relay.Close()
	g.relay = nil
}

// Open listens on the gate's address again, so dials reach the server once more. It does nothing
// when the gate is already open, and fails the test when the address cannot be taken.
func (g *Gate) Open() {
	g.t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.relay != nil {
		return
	}
	listener, err := net.Listen("tcp", g.address)
	if err != nil {
		g.t.Fatalf("open the gate at %s: %v", g.address, err)
		return
	}
	g.relay = newRelay(listener, g.target)
}

// NkeyGrant is a NATS test server, started by StartNkeyGranted, whose one nkey user may publish
// only to the subjects its grant names; Grant changes the grant.
type NkeyGrant struct {
	URL  string
	user string
	srv  *Server
}

// StartNkeyGranted runs a NATS server as StartNkeyAuthorized does, whose one user may publish
// only to the subjects allow names (and subscribe to anything): a publish to any other subject is
// the server's permissions violation, as a per-client grant makes it. The test can change the
// grant.
func StartNkeyGranted(t testing.TB, user string, allow ...string) *NkeyGrant {
	t.Helper()
	server, uri := startNkeyConfig(t, publishGrantConfig(user, allow))
	return &NkeyGrant{URL: uri, user: user, srv: server}
}

// Grant replaces the user's publish grant with allow and has the server reload its configuration,
// as an operator changing a grant does: the server keeps its connections and applies the new grant
// to them. It fails the test, with the server's reason, when the server refused the new
// configuration.
func (g *NkeyGrant) Grant(t testing.TB, allow ...string) {
	t.Helper()
	if err := g.srv.Reload(t, publishGrantConfig(g.user, allow)); err != nil {
		t.Fatalf("NATS refused the new grant %q: %v", allow, err)
	}
}

func publishGrantConfig(user string, allow []string) string {
	return grantConfig(user, "publish", allow)
}

// SubscribeGrantConfig is the configuration of a server with JetStream whose one user, the nkey user
// whose public key is user, may subscribe only to the subjects allow names, in any queue group, and
// publish to anything: a subscription to any other subject is the server's permissions violation,
// as a per-client grant makes it. StartConfig takes it, and Restart or Reload a changed one.
func SubscribeGrantConfig(user string, allow ...string) string {
	return grantConfig(user, "subscribe", allow)
}

// grantConfig is the configuration of a server with JetStream whose one nkey user may use direction
// (publish or subscribe) only on the subjects allow names.
func grantConfig(user, direction string, allow []string) string {
	quoted := make([]string, len(allow))
	for index, subject := range allow {
		quoted[index] = fmt.Sprintf("%q", subject)
	}
	return nkeyConfig(fmt.Sprintf("{ nkey: %q, permissions: { %s: { allow: [%s] } } }",
		user, direction, strings.Join(quoted, ", ")))
}

func nkeyConfig(entry string) string {
	return fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ %s ]\n}\n", entry)
}

// startNkeyConfig runs a NATS server with JetStream and the debug and protocol trace in its log,
// whose configuration is config: StartConfig with the arguments -DV -js. config must declare the
// users it enforces.
func startNkeyConfig(t testing.TB, config string) (*Server, string) {
	t.Helper()
	return StartConfig(t, config, []string{"-DV", "-js"}, false)
}

// StartConfig runs a NATS server whose whole configuration is config, with serverArgs as its
// command line after it (so -js for JetStream), shut down when the test ends (even when its start
// fails). It returns the server with its client URL once it answers a client with no credential
// (awaitAuth): with anonymous false, by refusing it; with anonymous true, by admitting it, as a
// configuration naming a no_auth_user makes it.
func StartConfig(t testing.TB, config string, serverArgs []string, anonymous bool) (*Server, string) {
	t.Helper()
	server := start(t, config, serverArgs)
	awaitAuth(t, server.URL(), anonymous)
	return server, server.URL()
}

// awaitAuth waits, within connectTimeout, for the server at uri to answer a connection with no
// credential the way its configuration says: to admit it when admit is set, as a server with a
// no_auth_user does, and otherwise to refuse it with its own authorization violation, the proof it
// enforces its nkey users. Either answer also proves it speaks the client protocol. A server can
// answer a dial before the NATS protocol handshake, and a dial then ends in EOF or a refusal that
// says nothing about the server's users, so that is retried. The opposite answer fails the test at
// once: a server that admits the connection enforces no users, and every refusal a test expects of
// it would pass for the wrong reason; one that refuses it has no no_auth_user.
func awaitAuth(t testing.TB, uri string, admit bool) {
	t.Helper()
	deadline := time.Now().Add(connectTimeout)
	var err error
	for time.Now().Before(deadline) {
		var conn *natsgo.Conn
		conn, err = natsgo.Connect(uri, natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
		switch {
		case err == nil:
			conn.Close()
			if admit {
				return
			}
			t.Fatalf("NATS at %s admitted a client with no credential: it enforces no nkey users", uri)
		case errors.Is(err, natsgo.ErrAuthorization):
			if !admit {
				return
			}
			t.Fatalf("NATS at %s refused a client with no credential: it has no no_auth_user", uri)
		}
		time.Sleep(retryInterval)
	}
	if admit {
		t.Fatalf("NATS at %s did not admit a client with no credential within %s: %v", uri, connectTimeout, err)
	}
	t.Fatalf("NATS at %s did not answer within %s: %v", uri, connectTimeout, err)
}

// ConnectSeed connects to uri as the nkey user whose seed is seed, or with no credential when seed
// is "", with a one-second dial timeout and no reconnect, then opts, and returns the connection or
// the server's answer: a test that expects a refusal reads it from the error.
func ConnectSeed(t testing.TB, uri, seed string, opts ...natsgo.Option) (*natsgo.Conn, error) {
	t.Helper()
	options := []natsgo.Option{natsgo.Timeout(dialTimeout), natsgo.NoReconnect()}
	if seed != "" {
		user, err := nkeys.FromSeed([]byte(seed))
		if err != nil {
			t.Fatalf("read the seed: %v", err)
		}
		public, err := user.PublicKey()
		if err != nil {
			t.Fatalf("read the seed's public key: %v", err)
		}
		options = append(options, natsgo.Nkey(public, user.Sign))
	}
	return natsgo.Connect(uri, append(options, opts...)...)
}

const (
	connectTimeout = 30 * time.Second
	retryInterval  = 100 * time.Millisecond
	dialTimeout    = time.Second
)

// Connect waits for a test NATS server to accept a real connection and JetStream to answer an
// operation before returning it. The relay accepts a connection before the server behind it has
// finished its protocol handshake, and NATS can complete that handshake while JetStream is still
// starting; a KV test needs both readiness boundaries.
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

// waitForJetStream retries a real JetStream operation rather than trusting a successful core-NATS
// connect. Kept separate so its transient boundary has a direct unit test.
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

// nats-server answers a stream create it could not make a file store for with streamStoreFailed,
// under the stream-create error code; RecreateKeyValue retries that answer for up to
// recreateTimeout.
const (
	streamStoreFailed     = "error creating store for stream"
	streamCreateErrorCode = natsgo.ErrorCode(10049)
	recreateTimeout       = 10 * time.Second
)

// RecreateKeyValue creates the KV bucket config names in place of one the test has just deleted,
// and returns it.
//
// nats-server answers a stream delete before it is done with the account's directories: a
// goroutine of its own then removes the account's streams directory, which it can only do once
// that directory is empty, so only when the deleted stream was the account's last (stream.go,
// stop, v2.10 through v2.15). A create that arrives before that goroutine has run can lose the
// directory between making it and making its stream's own inside it: the server logs "could not
// create storage directory - mkdir .../streams/<stream>: no such file or directory" and answers
// the create "error creating store for stream". No API says when the goroutine has run, so the
// create is retried on exactly that answer, every retryInterval; any other error fails the test
// at once.
func RecreateKeyValue(t testing.TB, js natsgo.JetStreamContext, config *natsgo.KeyValueConfig) natsgo.KeyValue {
	t.Helper()
	var kv natsgo.KeyValue
	err := retryStreamStoreFailure(func() error {
		var err error
		kv, err = js.CreateKeyValue(config)
		return err
	})
	if err != nil {
		t.Fatalf("recreate KV bucket %s: %v", config.Bucket, err)
	}
	return kv
}

// retryStreamStoreFailure runs create again while it fails with the server's stream store
// failure, for up to recreateTimeout, and returns its last error.
func retryStreamStoreFailure(create func() error) error {
	deadline := time.Now().Add(recreateTimeout)
	for {
		err := create()
		var apiErr *natsgo.APIError
		storeFailed := errors.As(err, &apiErr) && apiErr.ErrorCode == streamCreateErrorCode && apiErr.Description == streamStoreFailed
		if !storeFailed || time.Now().After(deadline) {
			return err
		}
		time.Sleep(retryInterval)
	}
}
