// packages/envoy/internal/broker/airlockproxy/proxy_test.go
//
// Every row here drives the real proxy: a real listener on loopback, a real HTTP client that
// points its proxy at it, real TLS in both directions, a stand-in service that records what it
// was sent, and a fake GrantSource standing in for the two reads of requests.Machine. Nothing is
// mocked at the HTTP layer, because what each row is about - what a command's own client sends,
// what the proxy swaps, what the service receives, what the command reads back - is exactly what
// a mock would have to decide for itself.
//
// The two seams a row drives are the ones the production code has: Options.Dial, which is where
// every upstream connection is made, and Server.resolve, which answers the addresses a name has.
// Together they let a public name reach a stand-in service on loopback without the loopback
// refusal standing in the way, and let a row say which addresses a name has when that refusal is
// what it is about.
package airlockproxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/requests"
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

// fakeGrants is the proxy's GrantSource: the two reads of requests.Machine, answered from maps a
// row writes, and a count of how often the values were read.
type fakeGrants struct {
	mu          sync.Mutex
	connections map[string]requests.AirlockConnection
	refusals    map[string]error
	values      map[string]map[string]string
	valuesErr   error
	valueCalls  map[string]int
	connCalls   int
}

func newFakeGrants() *fakeGrants {
	return &fakeGrants{
		connections: map[string]requests.AirlockConnection{workerPlaceholder: testConnection()},
		refusals:    map[string]error{},
		values:      map[string]map[string]string{testGrantID: {workerName: workerValue}},
		valueCalls:  map[string]int{},
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

func (f *fakeGrants) AirlockValues(_ context.Context, grantID string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valueCalls[grantID]++
	if f.valuesErr != nil {
		return nil, f.valuesErr
	}
	return f.values[grantID], nil
}

func (f *fakeGrants) valueReads(grantID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.valueCalls[grantID]
}

// seenRequest is one request the stand-in service received, as the rows read it.
type seenRequest struct {
	method, path, query, host string
	header                    http.Header
	body                      string
}

// logLine is one line the proxy logged, as the rows read it.
type logLine struct {
	message string
	level   slog.Level
	fields  map[string]any
}

// logCapture is the slog handler the rows install: every line, with its attributes, in order.
type logCapture struct {
	mu    sync.Mutex
	lines []logLine
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, record slog.Record) error {
	fields := map[string]any{}
	record.Attrs(func(a slog.Attr) bool {
		fields[a.Key] = a.Value.Any()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, logLine{message: record.Message, level: record.Level, fields: fields})
	return nil
}

func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// calls is every line one call to a bound host logged.
func (c *logCapture) calls() []logLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	var calls []logLine
	for _, line := range c.lines {
		if line.message == callMessage {
			calls = append(calls, line)
		}
	}
	return calls
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
	// dialTo is where the dial seam sends each authority.
	dialTo map[string]string
	dials  atomic.Int64

	mu      sync.Mutex
	seen    []seenRequest
	answer  func(w http.ResponseWriter, r *http.Request)
	service *httptest.Server
}

// newRig builds the whole world. Everything of o the caller leaves zero is filled in.
func newRig(t *testing.T, o Options) *rig {
	t.Helper()
	r := &rig{t: t, grants: newFakeGrants(), logs: &logCapture{}, trust: x509.NewCertPool()}

	// The stand-in service behind the bound host. httptest's own certificate names example.com,
	// which is why that is the bound host here.
	r.service = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.seen = append(r.seen, seenRequest{
			method: req.Method, path: req.URL.Path, query: req.URL.RawQuery, host: req.Host,
			header: req.Header.Clone(), body: string(body),
		})
		answer := r.answer
		r.mu.Unlock()
		if answer != nil {
			answer(w, req)
			return
		}
		fmt.Fprintln(w, "the service answered")
	}))
	r.service.StartTLS()
	t.Cleanup(r.service.Close)
	servicePool := x509.NewCertPool()
	servicePool.AddCert(r.service.Certificate())

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
	r.dialTo = map[string]string{
		net.JoinHostPort(boundHost, "443"):  r.service.Listener.Addr().String(),
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
	r.proxy = New(o)
	r.proxy.resolve = r.resolve
	// What the proxy trusts upstream: the stand-in service's own certificate, in place of the
	// host's roots. Nothing else about the upstream side is replaced.
	r.proxy.upstream.TLSClientConfig.RootCAs = servicePool

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

// dial is Options.Dial: every upstream connection of the proxy, counted and routed to whichever
// stand-in service the row means.
func (r *rig) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	r.dials.Add(1)
	to, ok := r.dialTo[addr]
	if !ok {
		return nil, fmt.Errorf("this test dials no %s", addr)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, to)
}

// resolve is Server.resolve: an address literal is itself, a name the row named has the
// addresses it was given, and any other name resolves to nothing.
func (r *rig) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr.Unmap()}, nil
	}
	if addrs, ok := r.addresses[host]; ok {
		return addrs, nil
	}
	return nil, fmt.Errorf("no such host")
}

// requests is what the stand-in service has been sent.
func (r *rig) requests() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]seenRequest(nil), r.seen...)
}

// client is a command's own HTTP client, pointed at the proxy with password as its proxy
// password and trusting the proxy's authority.
func (r *rig) client(password string) *http.Client {
	r.t.Helper()
	proxyURL, err := url.Parse("http://agent:" + password + "@" + r.addr)
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

// writeCAFiles writes one certificate authority's PEM certificate and key into a directory of
// its own and answers the two paths.
func writeCAFiles(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("the authority's key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "airlock test authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("the authority's certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("the authority's key, encoded: %v", err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
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

// TestACredentialThatOpensNothingIsRefusedAndTunnelsNothing: the proxy's password is a
// placeholder of a live grant, and every other credential - none, one that is not Basic, one
// that is not base64, one no row holds, one whose grant has ended, one whose names are all
// withheld - is 407 with nothing dialled. A proxy that tunnelled first and checked afterwards
// would be an open relay for anyone who can reach the pod.
func TestACredentialThatOpensNothingIsRefusedAndTunnelsNothing(t *testing.T) {
	r := newRig(t, Options{})
	ended := requests.PlaceholderPrefix + "endedendedendedendedendedended01"
	noNames := requests.PlaceholderPrefix + "nonamesnonamesnonamesnonames0001"
	r.grants.refusals[ended] = requests.ErrGrantNotLive
	r.grants.refusals[noNames] = requests.ErrAirlockNoNames

	for _, row := range []struct{ name, header, want string }{
		{"no credential at all", "", "carries none"},
		{"a credential that is not Basic", "Proxy-Authorization: Bearer " + workerPlaceholder + "\r\n", "not Basic"},
		{"a Basic credential that is not base64", "Proxy-Authorization: Basic not-base-64-at-all\r\n", "not base64"},
		{"a password no grant holds", basic(requests.PlaceholderPrefix + "unknownunknownunknownunknown001"), requests.ErrGrantNotLive.Error()},
		{"an empty password", basic(""), requests.ErrGrantNotLive.Error()},
		{"a grant that has ended", basic(ended), requests.ErrGrantNotLive.Error()},
		{"a grant holding no name this session may have", basic(noNames), requests.ErrAirlockNoNames.Error()},
	} {
		t.Run(row.name, func(t *testing.T) {
			before := r.dials.Load()
			answer, _, _ := r.connectRaw(net.JoinHostPort(tunnelHost, "443"), row.header)
			body := bodyOf(t, answer)
			if answer.StatusCode != http.StatusProxyAuthRequired {
				t.Fatalf("CONNECT with %s = %d %q; want 407: a connection whose credential opens nothing must open nothing", row.name, answer.StatusCode, body)
			}
			if got := answer.Header.Get("Proxy-Authenticate"); got != `Basic realm="airlock"` {
				t.Errorf("Proxy-Authenticate = %q; want Basic realm=\"airlock\", which is how a client knows to send its placeholder", got)
			}
			if !strings.Contains(body, row.want) {
				t.Errorf("the refusal reads %q; want it to name %q, the broker's own reason", body, row.want)
			}
			if strings.Contains(body, workerPlaceholder) || strings.Contains(body, ended) || strings.Contains(body, noNames) {
				t.Errorf("the refusal names a placeholder: %q", body)
			}
			if after := r.dials.Load(); after != before {
				t.Fatalf("the proxy dialled %d time(s) for a credential it refused; want none tunnelled", after-before)
			}
		})
	}
}

// TestABoundHostGetsTheValueSwappedIntoTheHeaderTheEntryNames is the airlock itself: the command
// holds a placeholder, the service receives the value, and what the command sent in any other
// place is sent on as it was.
func TestABoundHostGetsTheValueSwappedIntoTheHeaderTheEntryNames(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.do(client, http.MethodPost, "https://"+boundHost+"/v1/things?token="+workerPlaceholder, "the body holds "+workerPlaceholder,
		"Authorization", "Bearer "+workerPlaceholder,
		"X-Other", "carried: "+workerPlaceholder)
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("the call answered %d %q; want 200 from the service", answer.StatusCode, body)
	}
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want exactly one", len(seen))
	}
	got := seen[0]
	if want := "Bearer " + workerValue; got.header.Get("Authorization") != want {
		t.Errorf("the service saw Authorization %q; want %q: the entry names that header, so the value goes in it", got.header.Get("Authorization"), want)
	}
	// Everything the entry does not name travels as the command wrote it, placeholder and all:
	// the airlock swaps the headers of the entry and nothing else.
	if want := "carried: " + workerPlaceholder; got.header.Get("X-Other") != want {
		t.Errorf("the service saw X-Other %q; want %q: a header the entry does not name is not swapped", got.header.Get("X-Other"), want)
	}
	if want := "token=" + workerPlaceholder; got.query != want {
		t.Errorf("the service saw the query %q; want %q: the airlock changes no query", got.query, want)
	}
	if want := "the body holds " + workerPlaceholder; got.body != want {
		t.Errorf("the service saw the body %q; want %q: the airlock reads no body and changes none", got.body, want)
	}
	if got.host != boundHost {
		t.Errorf("the service saw Host %q; want %q, the CONNECT target's host", got.host, boundHost)
	}
	if got.method != http.MethodPost || got.path != "/v1/things" {
		t.Errorf("the service saw %s %s; want POST /v1/things", got.method, got.path)
	}
}

// TestARequestWhoseHostIsNotTheConnectTargetIsRefused: the CONNECT says where this connection
// goes and the Host header says which service is being addressed, so a request that disagrees is
// a request trying to carry the value somewhere the rules were never read for.
func TestARequestWhoseHostIsNotTheConnectTargetIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things",
		"Authorization", "Bearer "+workerPlaceholder,
		"Host", "elsewhere.example.com")
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "host header mismatch") {
		t.Fatalf("the call answered %d %q; want 403 naming the host header mismatch", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none: a refused request reaches no service", len(seen))
	}
}

// TestAMethodOrPathTheEntryDoesNotAllowIsRefused: the entry's allow list is a bound on where the
// value may go, and the proxy is where it is enforced.
func TestAMethodOrPathTheEntryDoesNotAllowIsRefused(t *testing.T) {
	for _, row := range []struct{ name, method, path string }{
		{"a method the entry does not allow", http.MethodDelete, "/v1/things"},
		{"a path the entry does not allow", http.MethodGet, "/admin/things"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			client := r.client(workerPlaceholder)
			answer, body := r.do(client, row.method, "https://"+boundHost+row.path, "", "Authorization", "Bearer "+workerPlaceholder)
			if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "not allowed by rule") {
				t.Fatalf("%s %s answered %d %q; want 403 naming the rule", row.method, row.path, answer.StatusCode, body)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Fatalf("the service was sent %d requests; want none", len(seen))
			}
		})
	}
}

// TestARequestThatAlreadyCarriesTheValueIsRefused: a command that holds the real value learnt it
// somewhere other than this swap, so the airlock refuses the request rather than carrying it.
func TestARequestThatAlreadyCarriesTheValueIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerValue)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "request already carries the value") {
		t.Fatalf("the call answered %d %q; want 403 naming that the request already carries the value", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none", len(seen))
	}
}

// TestPlainHTTPToABoundHostIsRefused: the value may travel only inside TLS the proxy itself
// opened, so an http:// request to a bound host is refused rather than forwarded.
func TestPlainHTTPToABoundHostIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "http://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, "refusing plain http") {
		t.Fatalf("plain http to a bound host answered %d %q; want 403 naming the rule", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none", len(seen))
	}
	if r.dials.Load() != 0 {
		t.Fatalf("the proxy dialled %d time(s) for a refused plain-http request; want none", r.dials.Load())
	}
}

// TestATunnelledHostKeepsItsOwnCertificateAndItsBytes: a host no name of the grant is bound to is
// a tunnel. The proxy opens no TLS on it - the command's own client verifies the service's own
// certificate, which the proxy's authority did not sign - and the bytes arrive as they were sent.
func TestATunnelledHostKeepsItsOwnCertificateAndItsBytes(t *testing.T) {
	r := newRig(t, Options{})
	answer, conn, _ := r.connectRaw(net.JoinHostPort(tunnelHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to a tunnelled host answered %d; want 200 Connection established", answer.StatusCode)
	}
	// The command's own client verifies the service's certificate, and the check below is that
	// the proxy's authority did not sign it: a proxy that had opened this connection would have
	// answered a leaf of its own.
	client := tls.Client(conn, &tls.Config{ServerName: tunnelHost, RootCAs: r.trust})
	if err := client.Handshake(); err != nil {
		t.Fatalf("the tunnelled handshake: %v", err)
	}
	peer := client.ConnectionState().PeerCertificates[0]
	if err := peer.CheckSignatureFrom(r.authority.Certificate()); err == nil {
		t.Fatal("the tunnelled service's certificate was signed by the proxy's own authority: the proxy opened a tunnel it was supposed to leave alone")
	}
	sent := "the bytes the command sent\n"
	if _, err := client.Write([]byte(sent)); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	read := make([]byte, len(sent))
	if _, err := io.ReadFull(client, read); err != nil {
		t.Fatalf("read back through the tunnel: %v", err)
	}
	if string(read) != sent {
		t.Fatalf("the tunnel answered %q; want %q", read, sent)
	}
	if calls := r.logs.calls(); len(calls) != 0 {
		t.Fatalf("the proxy logged %d call(s) to a bound host for a tunnel; want none: it reads nothing on a tunnel", len(calls))
	}
}

// TestTheProxyRefusesTheAddressesItMustNeverDial: without this the proxy is a way into the
// cluster the broker runs in. Each row names the rule and the address family in its refusal, and
// nothing is dialled.
func TestTheProxyRefusesTheAddressesItMustNeverDial(t *testing.T) {
	r := newRig(t, Options{})
	r.addresses["mixed.example.net"] = []netip.Addr{netip.MustParseAddr("93.184.216.40"), netip.MustParseAddr("10.9.9.9")}
	for _, row := range []struct{ host, want string }{
		{"127.0.0.1", "a loopback IPv4 address"},
		{"::1", "a loopback IPv6 address"},
		{"10.1.2.3", "a private IPv4 address"},
		{"172.16.3.4", "a private IPv4 address"},
		{"192.168.5.6", "a private IPv4 address"},
		{"fc00::1", "a unique-local IPv6 address"},
		{"169.254.169.254", "a link-local IPv4 address"},
		{"fe80::1", "a link-local IPv6 address"},
		{"100.64.1.2", "a cluster IPv4 address"},
		{"224.0.0.1", "a link-local IPv4 address"},
		{"239.1.2.3", "not a global unicast IPv4 address"},
		{"::", "not a global unicast IPv6 address"},
		{"mixed.example.net", "a private IPv4 address"},
	} {
		t.Run(row.host, func(t *testing.T) {
			before := r.dials.Load()
			answer, _, _ := r.connectRaw(net.JoinHostPort(row.host, "443"), basic(workerPlaceholder))
			body := bodyOf(t, answer)
			if answer.StatusCode != http.StatusForbidden {
				t.Fatalf("CONNECT %s = %d %q; want 403", row.host, answer.StatusCode, body)
			}
			if !strings.Contains(body, row.want) {
				t.Errorf("the refusal of %s reads %q; want it to name %q", row.host, body, row.want)
			}
			if after := r.dials.Load(); after != before {
				t.Fatalf("the proxy dialled %s %d time(s); want none: the refusal comes before any dial", row.host, after-before)
			}
		})
	}
}

// TestTheIdleLimitEndsAQuietConnectionAndKeepsABusyOne: a connection carries a grant's
// placeholders, so one a command walked away from is state the broker holds for nobody. A
// connection with bytes crossing it is not ended, however long it lives.
func TestTheIdleLimitEndsAQuietConnectionAndKeepsABusyOne(t *testing.T) {
	const idle = 400 * time.Millisecond
	t.Run("a connection with no byte is ended", func(t *testing.T) {
		r := newRig(t, Options{Idle: idle})
		answer, conn, reader := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT answered %d; want 200", answer.StatusCode)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		began := time.Now()
		if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) && err == nil {
			t.Fatalf("the quiet tunnel answered a byte after %v; want it closed", time.Since(began))
		} else if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("the quiet tunnel was still open %v after its %v idle limit", time.Since(began), idle)
		}
		if took := time.Since(began); took < idle {
			t.Fatalf("the tunnel ended after %v, before its %v idle limit had run out", took, idle)
		}
	})
	t.Run("a connection with bytes is kept", func(t *testing.T) {
		r := newRig(t, Options{Idle: idle})
		_, conn, reader := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
		// Three times the idle limit, a byte every tenth of it: a connection the limit ends here
		// is one whose clock a read or a write did not push out.
		deadline := time.Now().Add(3 * idle)
		for time.Now().Before(deadline) {
			if _, err := conn.Write([]byte("x")); err != nil {
				t.Fatalf("the busy tunnel was closed while it was carrying bytes: %v", err)
			}
			conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := reader.ReadByte(); err != nil {
				t.Fatalf("the busy tunnel was closed while it was carrying bytes: %v", err)
			}
			time.Sleep(idle / 10)
		}
	})
}

// TestShutdownClosesAnOpenTunnel: http.Server.Shutdown neither closes a hijacked connection nor
// waits for one, so without the proxy's own registry a drained pod would leave every tunnel open
// until the kubelet killed it.
func TestShutdownClosesAnOpenTunnel(t *testing.T) {
	r := newRig(t, Options{})
	answer, conn, reader := r.connectRaw(net.JoinHostPort(echoHost, "443"), basic(workerPlaceholder))
	if answer.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d; want 200", answer.StatusCode)
	}
	if _, err := conn.Write([]byte("open\n")); err != nil {
		t.Fatalf("write through the tunnel: %v", err)
	}
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("the tunnel was not carrying bytes before the drain: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.proxy.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := reader.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the tunnel was still open after the drain (%v); want it closed", err)
	}
}

// TestTheValuesOfOneGrantAreReadOnceOnThisPod: AirlockValues runs the full release checks - the
// record chain, the policy re-evaluation, the max-grant, the airlock fingerprint - so running it
// per request would put that whole read on every call a command makes.
func TestTheValuesOfOneGrantAreReadOnceOnThisPod(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	for range 2 {
		answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
		if answer.StatusCode != http.StatusOK {
			t.Fatalf("the call answered %d %q; want 200", answer.StatusCode, body)
		}
		// A second connection, not a second request on the first one: the read is once per
		// grant per pod, not once per connection.
		client.CloseIdleConnections()
	}
	if calls := r.grants.valueReads(testGrantID); calls != 1 {
		t.Fatalf("AirlockValues ran %d times for two connections of one grant; want once per grant per pod", calls)
	}
	if r.grants.connCalls < 2 {
		t.Fatalf("the proxy took %d connections; want two, so that the row is about two connections of one grant", r.grants.connCalls)
	}
}

// TestEachCallToABoundHostLogsOneLine: an operator reading the broker's log must be able to say
// which session carried which secret where, and on what issue's authority - and must never read
// a placeholder or a value there.
func TestEachCallToABoundHostLogsOneLine(t *testing.T) {
	r := newRig(t, Options{})
	client := r.client(workerPlaceholder)
	r.get(client, "https://"+boundHost+"/v1/things?token=secret-query", "Authorization", "Bearer "+workerPlaceholder)
	r.do(client, http.MethodDelete, "https://"+boundHost+"/v1/things", "", "Authorization", "Bearer "+workerPlaceholder)

	calls := r.logs.calls()
	if len(calls) != 2 {
		t.Fatalf("the proxy logged %d lines for two calls to a bound host; want one each", len(calls))
	}
	allowed, refused := calls[0], calls[1]
	for _, line := range calls {
		if line.level != slog.LevelInfo {
			t.Errorf("the call line is at %v; want info", line.level)
		}
		for field, want := range map[string]any{
			"enrollment": testEnrollID, "grant": testGrantID, "secret": workerName,
			"issue": testIssue, "host": boundHost, "path": "/v1/things",
		} {
			if got := line.fields[field]; got != want {
				t.Errorf("the call line's %s = %v; want %v", field, got, want)
			}
		}
		if fmt.Sprint(line.fields) != "" && strings.Contains(fmt.Sprint(line.fields), workerValue) {
			t.Errorf("the call line names the value: %v", line.fields)
		}
		if strings.Contains(fmt.Sprint(line.fields), workerPlaceholder) {
			t.Errorf("the call line names the placeholder: %v", line.fields)
		}
		if strings.Contains(fmt.Sprint(line.fields["path"]), "token=") {
			t.Errorf("the call line's path carries the query: %v", line.fields["path"])
		}
	}
	if allowed.fields["method"] != http.MethodGet || allowed.fields["allowed"] != true || allowed.fields["status"] != int64(200) {
		t.Errorf("the allowed call logged %v; want method GET, allowed true and status 200", allowed.fields)
	}
	if _, named := allowed.fields["reason"]; named {
		t.Errorf("the allowed call logged a reason: %v", allowed.fields)
	}
	if refused.fields["method"] != http.MethodDelete || refused.fields["allowed"] != false || refused.fields["status"] != int64(403) {
		t.Errorf("the refused call logged %v; want method DELETE, allowed false and status 403", refused.fields)
	}
	if reason, _ := refused.fields["reason"].(string); reason != "not allowed by rule" {
		t.Errorf("the refused call logged the reason %q; want the decision's own words", reason)
	}
}

// TestOneRequestIsHandledForOneNameOfTheGrant: two secrets of one grant can be bound to one host,
// and the decision is over one secret, so the request is handled for the secret it carries a
// placeholder of - and for the first candidate when it carries none, which is still decided and
// still forwarded with nothing swapped.
func TestOneRequestIsHandledForOneNameOfTheGrant(t *testing.T) {
	const (
		alphaName        = "ALPHA_TOKEN"
		betaName         = "BETA_TOKEN"
		alphaValue       = "the-real-alpha"
		betaValue        = "the-real-beta"
		alphaPlaceholder = requests.PlaceholderPrefix + "alphaalphaalphaalphaalphaalpha01"
		betaPlaceholder  = requests.PlaceholderPrefix + "betabetabetabetabetabetabetabe01"
	)
	rules := airlock.Rules{Secrets: []airlock.SecretRule{
		{Name: alphaName, Hosts: []string{boundHost}, Headers: []string{"authorization"}},
		{Name: betaName, Hosts: []string{boundHost}, Headers: []string{"x-beta"}},
	}}
	r := newRig(t, Options{Rules: rules})
	r.grants.connections[alphaPlaceholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			alphaName: {Slots: map[string]string{requests.SlotValue: alphaPlaceholder}},
			betaName:  {Slots: map[string]string{requests.SlotValue: betaPlaceholder}},
		},
	}
	r.grants.values[testGrantID] = map[string]string{alphaName: alphaValue, betaName: betaValue}
	client := r.client(alphaPlaceholder)

	// The request carries beta's placeholder and no other, so it is handled for beta: beta's
	// value goes into the header beta's entry names.
	r.get(client, "https://"+boundHost+"/v1/things", "X-Beta", "Token "+betaPlaceholder)
	seen := r.requests()
	if len(seen) != 1 {
		t.Fatalf("the service saw %d requests; want one", len(seen))
	}
	if want := "Token " + betaValue; seen[0].header.Get("X-Beta") != want {
		t.Fatalf("the service saw X-Beta %q; want %q: the request carried beta's placeholder, so it is handled for beta", seen[0].header.Get("X-Beta"), want)
	}
	if secret := r.logs.calls()[0].fields["secret"]; secret != betaName {
		t.Fatalf("the call logged the secret %v; want %s", secret, betaName)
	}

	// A request that carries no placeholder at all is still decided - for the first candidate in
	// name order - and forwarded with nothing swapped.
	r.get(client, "https://"+boundHost+"/v1/other", "X-Plain", "nothing of this grant")
	seen = r.requests()
	if len(seen) != 2 {
		t.Fatalf("the service saw %d requests; want two", len(seen))
	}
	if got := seen[1].header.Get("X-Plain"); got != "nothing of this grant" {
		t.Errorf("the service saw X-Plain %q; want it unchanged", got)
	}
	if secret := r.logs.calls()[1].fields["secret"]; secret != alphaName {
		t.Fatalf("the call logged the secret %v; want %s, the first candidate in name order", secret, alphaName)
	}

	// A request that carries both is handled for the first candidate, and for that one alone:
	// the decision is over one secret, so the other's placeholder travels on as the command
	// wrote it rather than being swapped beside it.
	r.get(client, "https://"+boundHost+"/v1/both", "Authorization", "Bearer "+alphaPlaceholder, "X-Beta", "Token "+betaPlaceholder)
	seen = r.requests()
	if len(seen) != 3 {
		t.Fatalf("the service saw %d requests; want three", len(seen))
	}
	if want := "Bearer " + alphaValue; seen[2].header.Get("Authorization") != want {
		t.Errorf("the service saw Authorization %q; want %q", seen[2].header.Get("Authorization"), want)
	}
	if want := "Token " + betaPlaceholder; seen[2].header.Get("X-Beta") != want {
		t.Errorf("the service saw X-Beta %q; want %q: one request is handled for one name", seen[2].header.Get("X-Beta"), want)
	}
}

// TestANameWhoseKindNeedsMoreThanAValueIsRefused: the kinds whose released value is more than one
// string - aws, azure-login, a github-app installation - are the next task's. Until then such a
// name is refused, naming its kind, rather than forwarded with the command's placeholder in it.
func TestANameWhoseKindNeedsMoreThanAValueIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	r.grants.connections[workerPlaceholder] = requests.AirlockConnection{
		GrantID: testGrantID, EnrollmentID: testEnrollID, RequestID: testRequest, Issue: testIssue,
		Names: map[string]requests.AirlockGrantName{
			workerName: {Kind: "azure-login", Slots: map[string]string{requests.SlotAzureFederated: workerPlaceholder}},
		},
	}
	client := r.client(workerPlaceholder)
	answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
	if answer.StatusCode != http.StatusForbidden || !strings.Contains(body, `kind "azure-login"`) {
		t.Fatalf("the call answered %d %q; want 403 naming the kind the airlock does not yet carry", answer.StatusCode, body)
	}
	if seen := r.requests(); len(seen) != 0 {
		t.Fatalf("the service was sent %d requests; want none", len(seen))
	}
	if r.grants.valueReads(testGrantID) != 0 {
		t.Fatalf("the proxy read the grant's values for a name it cannot carry; want no read")
	}
}

// TestAGrantTheReleaseChecksRefuseIsRefused: the full checks run once per grant per pod, and
// their refusal is this request's. A grant that is no longer live is 407, as the connection
// check's own refusals are; anything else is 403.
func TestAGrantTheReleaseChecksRefuseIsRefused(t *testing.T) {
	for _, row := range []struct {
		name string
		err  error
		want int
	}{
		{"a grant that is no longer live", fmt.Errorf("%w: AUTO_TOKEN's airlock entry changed since this grant was decided", requests.ErrGrantNotLive), http.StatusProxyAuthRequired},
		{"a check the broker could not take", errors.New("the record chain could not be verified"), http.StatusForbidden},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := newRig(t, Options{})
			r.grants.valuesErr = row.err
			client := r.client(workerPlaceholder)
			answer, body := r.get(client, "https://"+boundHost+"/v1/things", "Authorization", "Bearer "+workerPlaceholder)
			if answer.StatusCode != row.want || !strings.Contains(body, row.err.Error()) {
				t.Fatalf("the call answered %d %q; want %d naming the broker's own refusal", answer.StatusCode, body, row.want)
			}
			if seen := r.requests(); len(seen) != 0 {
				t.Fatalf("the service was sent %d requests; want none", len(seen))
			}
		})
	}
}

// TestARequestThatIsNoProxyRequestIsRefused: this port serves nothing of its own.
func TestARequestThatIsNoProxyRequestIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	answer, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + r.addr + "/healthz")
	if err != nil {
		t.Fatalf("GET the proxy port: %v", err)
	}
	body := bodyOf(t, answer)
	if answer.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not a proxy request") {
		t.Fatalf("an ordinary request to the proxy port answered %d %q; want 400", answer.StatusCode, body)
	}
}
