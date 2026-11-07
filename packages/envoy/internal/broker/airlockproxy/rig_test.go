// packages/envoy/internal/broker/airlockproxy/rig_test.go
//
// The world a row runs in, and nothing that asserts anything about it: the proxy on a loopback
// listener, the fake GrantSource behind it, the stand-in service behind the bound host, the two
// tunnelled services, the certificate authority the rows sign with, and the log they read back.
// proxy_test.go, aws_test.go, azure_test.go and upgrade_test.go each build one with newRig, and
// ca_test.go takes the authority helpers alone. Why the rig is built out of real listeners and
// real TLS rather than mocks is in proxy_test.go's own comment.
package airlockproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/airlock/airlocktest"
	"github.com/sjawhar/envoy/internal/broker/requests"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The grant every row but the two-name one is served by: one secret, bound to one host, whose
// value may travel in Authorization alone and only on /v1/.
const (
	boundHost    = "example.com"
	tunnelHost   = "tunnel.example.net"
	echoHost     = "echo.example.net"
	workerName   = "WORKER_TOKEN"
	workerValue  = "ghp-the-real-worker-token"
	testGrantID  = "grant-00000000"
	testEnrollID = "enrollment-00000000"
	testRequest  = "request-00000000"
	testIssue    = "INFRA-1352"
)

// workerPlaceholder is a placeholder in the shape the broker mints them, so a row that asserts a
// body never names the placeholder is asserting about the real thing.
var workerPlaceholder = requests.PlaceholderPrefix + "AaBbCcDdEeFfGgHhIiJjKkLlMmNnOoPp"

// testRules is the rules file every row but the two-name one runs under.
func testRules() airlock.Rules {
	return airlock.Rules{Secrets: []airlock.SecretRule{{
		Name:    workerName,
		Hosts:   []string{boundHost},
		Headers: []string{"authorization"},
		Allow:   []airlock.Allow{{Method: http.MethodGet, PathPrefix: "/v1/"}, {Method: http.MethodPost, PathPrefix: "/v1/"}},
	}}}
}

// testConnection is what the fake answers for the proxy's password: one live grant holding the
// one name above.
func testConnection() requests.AirlockConnection {
	return requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			workerName: {Slots: map[string]string{requests.SlotValue: workerPlaceholder}},
		},
	}
}

// storedValues is one grant's values as the fake answers them: one name, one value slot, no
// expiry, which is what a stored secret's release looks like.
func storedValues(name, value string) map[string]requests.AirlockSecret {
	return map[string]requests.AirlockSecret{name: {Slots: map[string]string{requests.SlotValue: value}}}
}

// fakeGrants is the proxy's GrantSource: the reads of requests.Machine, answered from maps a
// row writes, and a count of how often the values were read.
type fakeGrants struct {
	mu          sync.Mutex
	connections map[string]requests.AirlockConnection
	refusals    map[string]error
	values      map[string]map[string]requests.AirlockSecret
	valuesErr   error
	valueCalls  map[string]int
	connCalls   int
	// version is the policy version the broker holds now, which every read answers as the one
	// it ran under. A row moves it to play a policy change the broker took by its periodic
	// reload, which reaches a pod's held values no other way.
	version string
	// access is the placeholder each (grant, name, slot) of an exchange was minted, as the
	// broker's own table holds it: minted once and answered again after.
	access      map[string]string
	accessCalls int
}

func newFakeGrants() *fakeGrants {
	return &fakeGrants{
		connections: map[string]requests.AirlockConnection{workerPlaceholder: testConnection()},
		refusals:    map[string]error{},
		values:      map[string]map[string]requests.AirlockSecret{testGrantID: storedValues(workerName, workerValue)},
		valueCalls:  map[string]int{},
		version:     "policy-v1",
		access:      map[string]string{},
	}
}

func (f *fakeGrants) AirlockConnection(_ context.Context, placeholder string) (requests.AirlockConnection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connCalls++
	if err, ok := f.refusals[placeholder]; ok {
		return requests.AirlockConnection{}, err
	}
	conn, ok := f.connections[placeholder]
	if !ok {
		return requests.AirlockConnection{}, requests.ErrGrantNotLive
	}
	return conn, nil
}

func (f *fakeGrants) AirlockValues(_ context.Context, grantID string) (map[string]requests.AirlockSecret, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valueCalls[grantID]++
	if f.valuesErr != nil {
		return nil, "", f.valuesErr
	}
	// A copy, as the broker's own read answers a map of its own: a row that changes what the
	// next read answers must not change the map a held answer is still being read from.
	held := map[string]requests.AirlockSecret{}
	maps.Copy(held, f.values[grantID])
	return held, f.version, nil
}

func (f *fakeGrants) PolicyVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version
}

// setPolicyVersion is a policy change the broker took: the version every later read answers,
// and the one the proxy compares what it holds against.
func (f *fakeGrants) setPolicyVersion(version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = version
}

// setValues replaces what the next read of grantID answers.
func (f *fakeGrants) setValues(grantID string, values map[string]requests.AirlockSecret) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[grantID] = values
}

func (f *fakeGrants) valueReads(grantID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valueCalls[grantID]
}

// refuseConnection is what the broker answers for a placeholder from here on: the refusal a
// grant that has ended, been revoked or had its enrollment's lease lapse gets.
func (f *fakeGrants) refuseConnection(placeholder string, why error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusals[placeholder] = why
}

// setConnection replaces the grant a placeholder opens from here on, which is how a row withholds
// one name of a grant and leaves the rest.
func (f *fakeGrants) setConnection(placeholder string, conn requests.AirlockConnection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connections[placeholder] = conn
}

// connectionReads is how often a password has been read as a grant: once per CONNECT and once
// per request on a connection the proxy decrypted.
func (f *fakeGrants) connectionReads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connCalls
}

// AirlockAccessPlaceholder is the broker's mint of one access token's placeholder: the same
// string for one (grant, name, slot) however often it is asked for, and written into the
// grant's names, as the broker's own row is, so a connection opened after it carries it and one
// opened before does not.
func (f *fakeGrants) AirlockAccessPlaceholder(_ context.Context, grantID, name, slot string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessCalls++
	key := grantID + "\x00" + name + "\x00" + slot
	placeholder, minted := f.access[key]
	if !minted {
		placeholder = fmt.Sprintf("%saccessToken%021d", requests.PlaceholderPrefix, len(f.access))
		f.access[key] = placeholder
	}
	for password, conn := range f.connections {
		held, carries := conn.Names[name]
		if conn.GrantID != grantID || !carries {
			continue
		}
		// Copied rather than written to: a connection already open is reading the map it was
		// handed, and the row this mints reaches it only on its next connection.
		slots := maps.Clone(held.Slots)
		slots[slot] = placeholder
		names := maps.Clone(conn.Names)
		names[name] = requests.AirlockGrantName{Slots: slots, Kind: held.Kind}
		conn.Names = names
		f.connections[password] = conn
	}
	return placeholder, nil
}

// accessMints is how many exchanges asked the broker for a placeholder.
func (f *fakeGrants) accessMints() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessCalls
}

// seenRequest is one request the stand-in service received, as the rows read it: the record the
// stand-in package keeps, since the service behind the bound host is that package's.
type seenRequest = airlocktest.Request

// logLine is one line the proxy logged, as the rows read it.
type logLine struct {
	message string
	level   slog.Level
	fields  map[string]any
}

// logCapture is the slog handler the rows install: logtest.Recorder keeps every record with the
// attributes the proxy wrote it with, and the methods here are this package's own reading of
// those records.
type logCapture struct {
	logtest.Recorder
}

// calls is every line one call to a bound host logged. Records answers a slice of its own, so a
// row reads the lines written so far while the proxy goes on writing.
func (c *logCapture) calls() []logLine {
	var calls []logLine
	for _, record := range c.Records() {
		if record.Message != callMessage {
			continue
		}
		fields := map[string]any{}
		record.Attrs(func(a slog.Attr) bool {
			fields[a.Key] = a.Value.Any()
			return true
		})
		calls = append(calls, logLine{message: record.Message, level: record.Level, fields: fields})
	}
	return calls
}

// callsAtLeast is the same, once there are n of them. The proxy writes a call's line after its
// answer has been copied to the client, so a client whose own read of the body has returned can
// still be ahead of that write by a few instructions: a row that reads calls()[0] straight after
// its request passes on an idle machine and panics on a loaded one, which is how this wait came
// to be written. It fails the row rather than hanging when the line never comes.
func (c *logCapture) callsAtLeast(t *testing.T, n int) []logLine {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		calls := c.calls()
		if len(calls) >= n {
			return calls
		}
		if time.Now().After(deadline) {
			t.Fatalf("the proxy wrote %d lines for a call to a bound host; want at least %d", len(calls), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// rig is one row's whole world: the proxy, what it dials, what it may resolve, the stand-in
// service behind the bound host, two tunnel services, and the log.
type rig struct {
	t         *testing.T
	proxy     *Server
	addr      string
	grants    *fakeGrants
	logs      *logCapture
	authority *Authority

	// trust is what the command's own client trusts: the proxy's authority, so a bound host
	// verifies, and the service certificates of the two tunnel hosts.
	trust *x509.CertPool
	// addresses is what resolve answers, which is how a row says a name is public or private.
	addresses map[string][]netip.Addr
	// dialTo is where the dial seam sends each authority, by the name a row wrote it under.
	// The proxy dials the address it checked rather than the name (address.go), so the seam
	// reads the name back out of addresses: a row names its services and the rig does the rest.
	dialTo map[string]string
	dials  atomic.Int64
	// answers is what resolve answers for a name, one list per call and the last one again
	// for every call after: how a row drives a name that answers one address to the check and
	// another to a lookup after it, which is DNS rebinding.
	answersMu sync.Mutex
	answers   map[string][][]netip.Addr
	// dialed is every address the proxy opened a socket to, in order, which is how a row reads
	// where a dial went.
	dialedMu sync.Mutex
	dialed   []string

	service *airlocktest.Service
}

// newRig builds the whole world. Everything of o the caller leaves zero is filled in.
func newRig(t *testing.T, o Options) *rig {
	t.Helper()
	r := &rig{t: t, grants: newFakeGrants(), logs: &logCapture{}, trust: x509.NewCertPool()}

	// The stand-in service behind the bound host, with a certificate of its own naming that
	// host and an authority of the rig's own behind it, which is the only thing the proxy
	// trusts upstream.
	service, err := airlocktest.NewService(airlocktest.ServiceOptions{Hosts: []string{boundHost}})
	if err != nil {
		t.Fatalf("the stand-in service: %v", err)
	}
	r.service = service
	t.Cleanup(r.service.Close)

	// The two tunnelled services: one that speaks TLS with a certificate of its own, and one
	// plain echo the idle and drain rows run bytes through.
	tunnelCA := newTestAuthority(t)
	tunnelCertificate, err := tunnelCA.leaf(tunnelHost)
	if err != nil {
		t.Fatalf("the tunnel service's certificate: %v", err)
	}
	tunnelAddr := startTLSEcho(t, tunnelCertificate)
	echoAddr := startEcho(t)
	r.trust.AddCert(tunnelCA.Certificate())

	r.authority = newTestAuthority(t)
	r.trust.AddCert(r.authority.Certificate())

	r.addresses = map[string][]netip.Addr{
		boundHost:  {netip.MustParseAddr("93.184.216.34")},
		tunnelHost: {netip.MustParseAddr("93.184.216.35")},
		echoHost:   {netip.MustParseAddr("93.184.216.36")},
	}
	r.answers = map[string][][]netip.Addr{}
	r.dialTo = map[string]string{
		net.JoinHostPort(boundHost, "443"):  r.service.Addr(),
		net.JoinHostPort(tunnelHost, "443"): tunnelAddr,
		net.JoinHostPort(echoHost, "443"):   echoAddr,
	}

	if o.Rules.Secrets == nil {
		o.Rules = testRules()
	}
	o.Grants = r.grants
	o.CA = r.authority
	o.Log = slog.New(r.logs)
	o.Dial = r.dial
	o.Resolve = r.resolve
	// What the proxy trusts upstream: the stand-in service's own authority, in place of the
	// host's roots. Nothing else about the upstream side is replaced.
	o.UpstreamRoots = r.service.Authority().Pool()
	r.proxy = New(o)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the proxy port: %v", err)
	}
	r.addr = listener.Addr().String()
	go func() {
		if err := r.proxy.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the proxy stopped serving: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r.proxy.Shutdown(ctx)
	})
	return r
}

// dial is Options.Dial: every upstream connection of the proxy, counted, recorded and routed to
// whichever stand-in service the row means. What it is handed is an address and a port, the
// address the proxy resolved and checked, so the name it was resolved from is read back out of
// the rig's own answers.
func (r *rig) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	r.dials.Add(1)
	r.dialedMu.Lock()
	r.dialed = append(r.dialed, addr)
	r.dialedMu.Unlock()
	to, ok := r.dialTo[addr]
	if !ok {
		to, ok = r.dialTo[r.nameOf(addr)]
	}
	if !ok {
		return nil, fmt.Errorf("this test dials no %s", addr)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, to)
}

// nameOf is the authority a row wrote dialTo under for an address and port: the name whose
// addresses hold that address, with that port. Names are walked in order, so two names sharing
// one address answer the same way in every run.
func (r *rig) nameOf(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	for _, name := range slices.Sorted(maps.Keys(r.addresses)) {
		for _, held := range r.addresses[name] {
			if held.String() == host {
				return net.JoinHostPort(name, port)
			}
		}
	}
	return addr
}

// dialedAddresses is every address the proxy dialled so far.
func (r *rig) dialedAddresses() []string {
	r.dialedMu.Lock()
	defer r.dialedMu.Unlock()
	return slices.Clone(r.dialed)
}

// resolve is Server.resolve: an address literal is itself, a name a row queued answers for has
// them one per call, a name the row named has the addresses it was given, and any other name
// resolves to nothing.
func (r *rig) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	r.answersMu.Lock()
	queued, ok := r.answers[host]
	if ok && len(queued) > 0 {
		next := queued[0]
		if len(queued) > 1 {
			// The last answer stands for every lookup after it, so a row names one answer
			// for the check and one for everything the name is looked up for afterwards.
			r.answers[host] = queued[1:]
		}
		r.answersMu.Unlock()
		return next, nil
	}
	r.answersMu.Unlock()
	if addrs, ok := r.addresses[host]; ok {
		return addrs, nil
	}
	return nil, fmt.Errorf("no such host")
}

// requests is what the stand-in service has been sent.
func (r *rig) requests() []seenRequest { return r.service.Requests() }

// answerWith is what the stand-in service answers from here on, in place of its one line: a row
// whose point is what the service received - the signature it verifies, the token it is given,
// the upgrade it answers - writes that answer here.
func (r *rig) answerWith(answer airlocktest.Answer) {
	r.service.AnswerWith(answer)
}

// client is a command's own HTTP client, pointed at the rig's proxy with password as its proxy
// password and trusting the proxy's authority. Each call makes a client of its own, so two of
// them are two connections, which is what a command's second call after a pod minted something
// for it is.
func (r *rig) client(password string) *http.Client {
	r.t.Helper()
	return r.clientAt(r.addr, password)
}

// clientAt is that client pointed at one proxy address, which a row with a second pod of its
// own needs.
func (r *rig) clientAt(addr, password string) *http.Client {
	r.t.Helper()
	proxyURL, err := url.Parse("http://agent:" + password + "@" + addr)
	if err != nil {
		r.t.Fatalf("the proxy URL: %v", err)
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: r.trust},
		},
	}
}

// get is one GET on a bound host through the proxy, with the headers given as name/value pairs.
func (r *rig) get(client *http.Client, target string, headers ...string) (*http.Response, string) {
	r.t.Helper()
	return r.do(client, http.MethodGet, target, "", headers...)
}

// do is one request through the proxy. target is a whole URL; a body is sent when one is given.
func (r *rig) do(client *http.Client, method, target, body string, headers ...string) (*http.Response, string) {
	r.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		r.t.Fatalf("build the request: %v", err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			request.Host = headers[i+1]
			continue
		}
		request.Header.Set(headers[i], headers[i+1])
	}
	answer, err := client.Do(request)
	if err != nil {
		r.t.Fatalf("%s %s through the proxy: %v", method, target, err)
	}
	defer answer.Body.Close()
	read, err := io.ReadAll(answer.Body)
	if err != nil {
		r.t.Fatalf("read the answer: %v", err)
	}
	return answer, string(read)
}

// connectRaw writes one CONNECT on a connection of its own and answers what came back, with the
// connection still open: the rows about the credential, the tunnel and the idle limit each need
// the bytes rather than an HTTP client's reading of them. Each header must end in CRLF.
func (r *rig) connectRaw(target string, headers ...string) (*http.Response, net.Conn, *bufio.Reader) {
	r.t.Helper()
	conn, err := net.Dial("tcp", r.addr)
	if err != nil {
		r.t.Fatalf("dial the proxy: %v", err)
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n" + strings.Join(headers, "") + "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		r.t.Fatalf("write the CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	answer, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		r.t.Fatalf("read the answer to the CONNECT: %v", err)
	}
	r.t.Cleanup(func() { conn.Close() })
	return answer, conn, reader
}

// basic is one Proxy-Authorization header line, as a command's client writes it.
func basic(password string) string {
	return "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("agent:"+password)) + "\r\n"
}

// bodyOf reads and closes an answer's body.
func bodyOf(t *testing.T, answer *http.Response) string {
	t.Helper()
	defer answer.Body.Close()
	body, err := io.ReadAll(answer.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	return string(body)
}

// newTestAuthority is a certificate authority of its own: a key and certificate written to PEM
// files and read back through the production reader, so every row signs with the same code a pod
// does.
func newTestAuthority(t *testing.T) *Authority {
	t.Helper()
	certFile, keyFile := writeCAFiles(t)
	authority, err := NewAuthority(certFile, keyFile)
	if err != nil {
		t.Fatalf("the test certificate authority: %v", err)
	}
	return authority
}

// writeCAFiles makes one certificate authority through the stand-in package and writes its PEM
// certificate and key into a directory of its own, which is the pair a pod mounts.
func writeCAFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	authority, err := airlocktest.NewAuthority("airlock test authority")
	if err != nil {
		t.Fatalf("the authority: %v", err)
	}
	certFile, keyFile, err = authority.WriteFiles(t.TempDir())
	if err != nil {
		t.Fatalf("write the authority's files: %v", err)
	}
	return certFile, keyFile
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// startEcho is a plain TCP service that writes back whatever it is sent.
func startEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind the echo service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

// startTLSEcho is the same service behind a TLS certificate of its own, which is how the tunnel
// row proves the proxy opened nothing.
func startTLSEcho(t *testing.T, certificate *tls.Certificate) string {
	t.Helper()
	certificates := &tls.Config{Certificates: []tls.Certificate{*certificate}}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", certificates)
	if err != nil {
		t.Fatalf("bind the TLS echo service: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}
