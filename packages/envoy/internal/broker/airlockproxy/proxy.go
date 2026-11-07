// Package airlockproxy is the broker's proxy port: the airlock itself. A command the broker
// released a name behind the airlock to holds a placeholder where the secret would be and
// points its HTTPS_PROXY here; this port holds the real value and puts it into each request the
// rules allow, as that request leaves.
//
// One connection is one live grant. The proxy's password is a placeholder of that grant
// (requests.Machine.AirlockConnection), and the connection may carry every airlock name the
// grant holds that its operator has not withheld. A CONNECT to a host one of those names is
// bound to is answered with a certificate this proxy's own authority signs, so each request on
// it is read: the decision and the swap are internal/broker/airlock's Decide and Rewrite, which
// a Lean model is proved against and a differential fuzz test ties to, and nothing here decides
// or swaps anything itself. A CONNECT to any other host is a plain tunnel: the proxy opens no
// TLS, reads nothing, and copies bytes.
//
// What this package adds to those two functions is everything around them: who the connection
// is (the grant), where it may dial (address.go), what the request looks like as a
// airlock.Request, which (name, slot) pair of a grant one request is handled for, where the
// real values come from, how long they are held and what ends them, and the line each call to a
// bound host logs.
package airlockproxy

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/broker/airlock"
	"github.com/sjawhar/envoy/internal/broker/requests"
)

// readHeaderTimeout bounds how long a connection on this port may take to send the head of a
// request. It is the one deadline either server here carries: a request deadline or a write
// timeout would cut a streamed answer - a long poll, a large artifact, a server-sent event
// stream - in the middle, and what bounds a connection that has gone quiet instead is the idle
// limit (BROKER_AIRLOCK_IDLE_SECONDS), which a streamed answer keeps pushing out.
const readHeaderTimeout = 30 * time.Second

// upstreamIdleTimeout and upstreamIdleConns are how long an upstream connection is kept for the
// next request to a service and how many per host. The proxy speaks to a handful of API hosts
// for every grant, so keeping a connection is the difference between one TLS handshake per pod
// and one per call.
const (
	upstreamIdleTimeout = 90 * time.Second
	upstreamIdleConns   = 32
)

// GrantSource is the two reads of requests.Machine the proxy makes: one per connection, and one
// per grant per pod.
type GrantSource interface {
	// AirlockConnection is what one connection's password proves: the live grant behind it and
	// every name it may carry. Its refusals are this port's 407s.
	AirlockConnection(ctx context.Context, placeholder string) (requests.AirlockConnection, error)
	// AirlockValues runs the full release checks and answers the real value of every slot of
	// every name, with when each name must be read or minted again. The proxy calls it on a
	// grant's first connection on this pod and keeps what it answered.
	AirlockValues(ctx context.Context, grantID string) (map[string]requests.AirlockSecret, error)
	// AirlockAccessPlaceholder is the placeholder that stands for one access token the proxy
	// exchanged a grant's federated token for (azure.go): minted on the first exchange of that
	// name and slot and answered again after, so a command that asks for one scope twice - and
	// a second pod that exchanges that scope for itself - reads back the same string.
	AirlockAccessPlaceholder(ctx context.Context, grantID, name, slot string) (string, error)
}

// Options is what a Server is built from.
type Options struct {
	// Rules is the airlock's rules file, read once at boot: which hosts each secret may reach
	// and in which headers.
	Rules airlock.Rules
	// Grants is the broker's own two reads, in production a *requests.Machine.
	Grants GrantSource
	// CA signs the certificate of each bound host.
	CA *Authority
	// Idle is BROKER_AIRLOCK_IDLE_SECONDS: how long one connection may go with no byte in
	// either direction. Zero or less leaves a connection open for as long as the command holds
	// it.
	Idle time.Duration
	// Log is where the line for each call to a bound host goes. Nil is slog.Default().
	Log *slog.Logger
	// Dial is how every upstream connection is made; nil is a net.Dialer. It is handed the
	// address the proxy resolved and checked, never a host name, so a replacement of it
	// cannot reach a name's second answer (address.go). A test replaces it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now is the clock the proxy reads to tell a minted value that is about to stop working
	// from one that is not (mintAgainBefore); nil is time.Now. A test drives it rather than
	// waiting out a token's life.
	Now func() time.Time
	// Resolve answers the addresses a host has: the ones the refusal reads and the ones the
	// dial opens a socket to (checkedAddresses); nil is net.DefaultResolver's lookup. So a
	// host that resolves here reaches whatever this answers for it, and nothing below looks
	// the name up a second time. A test drives a public name at a stand-in service on
	// loopback with it, and BROKER_AIRLOCK_TEST_RESOLVE is it in the broker's local stack,
	// where a client that resolves a real name - az, the AWS client - has to reach a
	// stand-in.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// UpstreamRoots are the roots the proxy verifies a service's certificate against on its
	// own calls upstream; nil is the host's own roots, as any client uses. A test puts its
	// stand-in service's certificate in it, and BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE is it in
	// the broker's local stack, where the service behind a bound host is a stand-in with a
	// certificate of the run's own.
	UpstreamRoots *x509.CertPool
	// DialAllow is the host:port addresses the proxy may dial although the address they
	// resolve to is one it never dials - a loopback or private address. It is a list rather
	// than a switch because the broker's local stack needs both halves at once: the stand-in
	// services on loopback reachable, and the broker's own loopback port still refused.
	// BROKER_AIRLOCK_TEST_DIAL_ALLOW is it.
	DialAllow []string
	// KeepValues keeps a grant's values across the reread of one of its names and past the
	// five-minute margin of a minted token's expiry, so a run can show what the airlock does
	// when neither happens. BROKER_AIRLOCK_TEST_KEEP_VALUES is it, the negative control of
	// the check that both halves belong to.
	KeepValues bool
	// NoKind turns the azure exchange and the AWS signing off, so an entry whose kind names
	// one of them takes the plain header swap instead. BROKER_AIRLOCK_TEST_NO_KIND is it, the
	// negative control of the check the two kinds belong to.
	NoKind bool
}

// Server is the broker's proxy port: one HTTP CONNECT proxy whose password is a placeholder of a
// live grant.
type Server struct {
	rules  airlock.Rules
	grants GrantSource
	ca     *Authority
	idle   time.Duration
	log    *slog.Logger
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	now    func() time.Time

	// resolve answers the addresses a host has, the ones refuseLocalAddress reads. It is
	// Options.Resolve, and net.DefaultResolver's lookup for a proxy given none.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// dialAllow, keepValues and noKind are the three test-only settings of Options, each
	// refused on a production start of the broker (cmd/broker's
	// refuseAirlockTestSettingsInProduction).
	dialAllow  []string
	keepValues bool
	noKind     bool

	// outer serves the bound listener: CONNECT and absolute-URL proxy requests. inner serves
	// what the outer one decrypted, through innerLn.
	outer   *http.Server
	inner   *http.Server
	innerLn *chanListener
	// upstream carries every request to a service. Its TLSClientConfig trusts the host's roots
	// as any client does, or Options.UpstreamRoots when a caller named roots of its own.
	upstream *http.Transport
	serveOne sync.Once

	// conns is the CONNECT target and the grant of each decrypted connection, which
	// ConnContext reads into the context of every request on it.
	conns sync.Map // net.Conn -> connInfo

	// open is every hijacked connection: each tunnel (as its two ends) and each bound
	// connection. The drain closes them, since http.Server.Shutdown does not.
	openMu sync.Mutex
	open   map[io.Closer]struct{}

	// values is each grant's real values, read once per grant on this pod and held until the
	// reread of one of its names drops it (DropName) or a minted value nears its expiry
	// (grantValues, mintAgainBefore).
	valuesMu sync.Mutex
	values   map[string]*grantValues

	// access is each access token this pod exchanged a grant's federated token for, by grant,
	// name and slot (azure.go). It is this pod's own: the placeholder that stands for one of
	// them is the whole grant's, and a pod holding none for a slot exchanges for itself.
	accessMu sync.Mutex
	access   map[accessKey]*accessHeld
}

// connInfo is what a decrypted connection carries into each request on it: the CONNECT target
// it was opened for, and the proxy password it was opened with, which each request reads the
// grant again with.
type connInfo struct {
	// target is the CONNECT authority, always with a port; host is its host alone, ASCII
	// lower-cased, which is the airlock.Request's ConnectHost, and port is that authority's
	// port as a number, which is its ConnectPort. A bound connection is opened on port 443
	// alone (the candidates of any other port are none), and the port travels here all the
	// same so that the decision reads the port the CONNECT named rather than taking it on
	// trust.
	target string
	host   string
	port   uint16
	// password is the proxy password this connection was opened with: a placeholder of the
	// grant. Every request on the connection reads the grant again with it (serveBound), so a
	// grant that has ended, been revoked or had one of its names withheld since the CONNECT
	// stops at the next request rather than at the idle limit.
	password string
	// grant is what that password proved when the connection was opened. Nothing is decided
	// from it - each request is decided from the grant it read for itself - and what it is
	// here for is the line a request logs before that read answers.
	grant requests.AirlockConnection
}

// connKey is the context key connInfo travels under.
type connKey struct{}

// New builds the proxy. Nothing is bound and nothing is served until Serve.
func New(o Options) *Server {
	s := &Server{
		rules:      o.Rules,
		grants:     o.Grants,
		ca:         o.CA,
		idle:       o.Idle,
		log:        o.Log,
		dial:       o.Dial,
		now:        o.Now,
		resolve:    o.Resolve,
		dialAllow:  o.DialAllow,
		keepValues: o.KeepValues,
		noKind:     o.NoKind,
		innerLn:    newChanListener(),
		open:       map[io.Closer]struct{}{},
		values:     map[string]*grantValues{},
		access:     map[accessKey]*accessHeld{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.resolve == nil {
		s.resolve = ResolveHost
	}
	if s.dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		s.dial = dialer.DialContext
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.upstream = &http.Transport{
		// The whole check-and-dial, so that a request to a bound host reaches the address
		// this proxy resolved and checked for the name and no other: net/http hands this the
		// host name, which dialChecked resolves once, refuses as a whole and then dials. The
		// TLS server name stays the host name, which is what the service's certificate is
		// read against.
		DialContext:         s.dialChecked,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.UpstreamRoots},
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: upstreamIdleConns,
		IdleConnTimeout:     upstreamIdleTimeout,
	}
	// net/http writes a line of its own for a connection that fails before a handler sees it;
	// routing it through the broker's logger keeps it out of a bare stderr write.
	errorLog := log.New(slogWriter{log: s.log}, "", 0)
	s.outer = &http.Server{
		Handler:           http.HandlerFunc(s.serveProxy),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       o.Idle,
		ErrorLog:          errorLog,
	}
	s.inner = &http.Server{
		Handler:           http.HandlerFunc(s.serveBound),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       o.Idle,
		ErrorLog:          errorLog,
		// The CONNECT target and the grant of the connection this request arrived on. The
		// request's own Host header is checked against that target by airlock.Decide, so a
		// request that names another host is refused rather than carried.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			info, _ := s.conns.Load(c)
			return context.WithValue(ctx, connKey{}, info)
		},
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				s.conns.Delete(c)
				s.forget(c)
			}
		},
	}
	return s
}

// Serve takes the proxy port's listener, and the inner server with it. It returns what
// http.Server.Serve returns, http.ErrServerClosed for a drained proxy among it.
func (s *Server) Serve(ln net.Listener) error {
	s.serveOne.Do(func() {
		go func() {
			// The inner server ends with the channel listener the drain closes, so its own
			// ErrServerClosed and net.ErrClosed are the drain and say nothing.
			_ = s.inner.Serve(s.innerLn)
		}()
	})
	return s.outer.Serve(ln)
}

// Shutdown drains the proxy inside ctx: both servers, and then every hijacked connection, which
// http.Server.Shutdown neither closes nor waits for. A command whose tunnel is open when the pod
// drains therefore loses it at the end of the shutdown rather than at the kubelet's kill.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.outer.Shutdown(ctx)
	s.innerLn.Close()
	if innerErr := s.inner.Shutdown(ctx); err == nil {
		err = innerErr
	}
	s.closeOpen()
	return err
}

// serveProxy is the whole of the outer server: a proxy request, a credential that proves a live
// grant, and then either a tunnel or a bound host.
func (s *Server) serveProxy(w http.ResponseWriter, r *http.Request) {
	// A request that is neither CONNECT nor an absolute-URL proxy request is somebody treating
	// this port as an ordinary web server. It serves nothing of its own, not even a health
	// endpoint: the API port is where the broker answers.
	if r.Method != http.MethodConnect && !r.URL.IsAbs() {
		http.Error(w, "airlock: not a proxy request: this port takes CONNECT and absolute-URL proxy requests alone", http.StatusBadRequest)
		return
	}
	placeholder, why := proxyPassword(r)
	if why != "" {
		s.refuseCredential(w, why)
		return
	}
	grant, err := s.grants.AirlockConnection(r.Context(), placeholder)
	if err != nil {
		// The broker's own words for why this credential opens nothing - an unknown or ended
		// grant, an enrollment whose lease lapsed, a grant every one of whose names is
		// withheld - and never the placeholder itself, which a command's own proxy error line
		// would otherwise print and keep.
		s.refuseCredential(w, err.Error())
		return
	}
	if r.Method == http.MethodConnect {
		s.serveConnect(w, r, placeholder, grant)
		return
	}
	s.servePlain(w, r, grant)
}

// proxyPassword is the placeholder a proxy request authenticates with: the password of its
// Proxy-Authorization Basic credential. The user name is not read - a command puts anything
// there, and several of the measured clients put the secret's own name - and the second answer
// is why there is no password, which the caller answers 407.
func proxyPassword(r *http.Request) (string, string) {
	credential := r.Header.Get("Proxy-Authorization")
	if credential == "" {
		return "", "this port takes a Proxy-Authorization: Basic credential whose password is a placeholder of a live grant, and this request carries none"
	}
	scheme, payload, ok := strings.Cut(credential, " ")
	if !ok || !strings.EqualFold(scheme, "basic") {
		return "", "the Proxy-Authorization credential is not Basic"
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.Trim(payload, " \t"))
	if err != nil {
		return "", "the Basic credential is not base64"
	}
	_, password, _ := strings.Cut(string(decoded), ":")
	return password, ""
}

// refuseCredential answers 407 and asks for the credential again. Nothing is tunnelled and
// nothing is dialled: a request that reaches here never left the broker.
func (s *Server) refuseCredential(w http.ResponseWriter, why string) {
	w.Header().Set("Proxy-Authenticate", `Basic realm="airlock"`)
	http.Error(w, "airlock: "+why, http.StatusProxyAuthRequired)
}

// serveConnect answers one CONNECT: a host one of the grant's names is bound to is decrypted
// and read, any other host is a tunnel. Both are refused before any dial when the host resolves
// somewhere this proxy never goes.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request, placeholder string, grant requests.AirlockConnection) {
	target := r.Host
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}
	// The authority, with its host ASCII lower-cased: it is the URL and the Host header of
	// every request this connection carries, and the one string the entry's hosts and the
	// request's own Host header are compared against.
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		http.Error(w, "airlock: "+r.Host+" is no host and port", http.StatusBadRequest)
		return
	}
	host = airlock.ASCIILower(host)
	target = net.JoinHostPort(host, port)
	// The addresses this name has, read and checked once: the tunnel below dials these and
	// resolves nothing of its own, and a bound host's requests are checked and dialled the
	// same way by the transport they go out on (dialChecked).
	addrs, err := s.checkedAddresses(r.Context(), host, port)
	if err != nil {
		http.Error(w, "airlock: "+err.Error(), http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "airlock: this connection cannot be taken over", http.StatusInternalServerError)
		return
	}
	// Whether this CONNECT may be decrypted and read: the (name, slot) pairs of the grant
	// whose entry binds this host on the port the CONNECT named. One answer from the one
	// function that says what is bound (candidates), so a change to what is bound is made in
	// one place.
	//
	// An empty answer is a tunnel rather than a refusal, whether it is a host no name binds or
	// another port of a bound name: the airlock blocks nothing it does not bind (R3), the
	// proxy reads nothing on such a connection and puts nothing into it, and the command
	// reaches that service exactly as it reaches every host the rules say nothing about. The
	// port is in it because every name the decision compares - the CONNECT host, the TLS name
	// and the Host header - carries no port, so 443 is the only port an entry's hosts speak
	// about and another port of one name is another service.
	connectPort := connectPortOf(port)
	if len(s.candidates(grant, host, connectPort)) > 0 {
		s.connectBound(hijacker, placeholder, grant, target, host, connectPort)
		return
	}
	s.connectTunnel(w, r, hijacker, target, addrs, port)
}

// connectPortOf is a CONNECT authority's port as a number, and 0 for a port that is no
// number at all ("https") or does not fit in 16 bits. Only 443 is a bound host's port, so a
// port this cannot read is a tunnel's.
func connectPortOf(port string) uint16 {
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(number)
}

// connectBound answers a CONNECT to a host one of the grant's names is bound to, on port 443:
// the proxy opens the TLS itself, with a certificate of its own authority, and hands the
// decrypted connection to the inner server. What travels with the connection is the target and
// the password it was opened with: each request on it reads the grant again with that password
// and builds its own candidates from what that read answers (serveBound), so a grant that ends
// or a name that is withheld between two requests stops at the second one.
func (s *Server) connectBound(hijacker http.Hijacker, placeholder string, grant requests.AirlockConnection, target, host string, port uint16) {
	client, err := hijack(hijacker)
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		return
	}
	decrypted := tls.Server(newIdleConn(client, s.idle), &tls.Config{
		// http/1.1 alone, and deliberately: an HTTP/2 request carries its authority in the
		// :authority pseudo-header, which net/http puts in Host, but it also lets one
		// connection carry requests for several authorities. Offering only HTTP/1.1 keeps one
		// connection to one host, so the Host check airlock.Decide makes covers every request
		// that crosses it.
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			// The CONNECT target's host, not the ClientHello's name: a client that sends no
			// SNI still gets the certificate of the host it asked for, and a client whose SNI
			// differs from its CONNECT is refused by the decision rather than served a
			// certificate for the name it smuggled in.
			return s.ca.leaf(host)
		},
	})
	connection := net.Conn(decrypted)
	s.conns.Store(connection, connInfo{target: target, host: host, port: port, password: placeholder, grant: grant})
	s.track(connection)
	if !s.innerLn.hand(connection) {
		// The drain closed the listener between the hijack and here.
		s.conns.Delete(connection)
		s.forget(connection)
		connection.Close()
	}
}

// connectTunnel answers a CONNECT to every other host: bytes in both directions and nothing
// read. The dial comes before the hijack, so a service that cannot be reached is a 502 the
// command's own HTTP client reports, rather than a tunnel that opens and then dies. It dials
// the addresses serveConnect checked, in order, and resolves nothing of its own.
func (s *Server) connectTunnel(w http.ResponseWriter, r *http.Request, hijacker http.Hijacker, target string, addrs []netip.Addr, port string) {
	upstream, err := s.dialAddrs(r.Context(), "tcp", addrs, port)
	if err != nil {
		http.Error(w, "airlock: dial "+target+": "+err.Error(), http.StatusBadGateway)
		return
	}
	client, err := hijack(hijacker)
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	// The idle limit on both sides, so that neither end of the tunnel outlives it. The
	// command's side alone would not: once that side has half-closed, the copy below sits in
	// a read of the upstream connection, which a service that answers nothing and closes
	// nothing never ends, and closing the command's side would not wake it. Both wrappers see
	// the same bytes at the same moment - every byte read from one end is written to the
	// other - so the window is the one window, counted twice.
	bounded := newIdleConn(client, s.idle)
	boundedUpstream := newIdleConn(upstream, s.idle)
	ends := bothEnds{client: bounded, upstream: boundedUpstream}
	s.track(ends)
	// The one end of this tunnel, whichever copy reaches it first: both connections closed and
	// the registry's entry dropped, so nothing of a tunnel outlives the copy that ended it.
	var closed sync.Once
	end := func() {
		closed.Do(func() {
			ends.Close()
			s.forget(ends)
		})
	}
	go func() {
		if _, err := io.Copy(boundedUpstream, bounded); err != nil {
			// The command's side is gone, or the idle limit closed it: there is nothing left
			// to carry in either direction.
			end()
			return
		}
		// A command that has said everything it has to say - git's upload-pack, a shell
		// pipeline - closes its half and waits for the answer, so the half-close is passed on
		// rather than turned into a close of the whole tunnel. What bounds the half-closed
		// state is the upstream's own idle limit above: the service has that long to send
		// something, and each byte it sends pushes the window out again.
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
			return
		}
		end()
	}()
	go func() {
		_, _ = io.Copy(bounded, boundedUpstream)
		end()
	}()
}

// servePlain answers an absolute-URL proxy request, which is plain HTTP: a command that asked
// for http:// rather than https://.
func (s *Server) servePlain(w http.ResponseWriter, r *http.Request, grant requests.AirlockConnection) {
	host := airlock.ASCIILower(r.URL.Hostname())
	if s.boundName(grant, host) {
		// Plain HTTP to a bound host is refused rather than swapped into: the value would
		// cross the network in the clear, and no rule of the airlock says a secret may. The
		// command asks again over https, where the proxy's own certificate covers it.
		http.Error(w, "airlock: refusing plain http to "+host+", a host behind the airlock: a secret of this grant may reach it only inside TLS, so ask for it over https", http.StatusForbidden)
		return
	}
	// The name's addresses, checked before anything is forwarded so that the refusal is a 403
	// of this proxy's rather than the transport's 502. The transport below checks and dials
	// the name again (dialChecked), which is what makes the address it opens a socket to one
	// it checked itself.
	if _, err := s.checkedAddresses(r.Context(), host, cmp.Or(r.URL.Port(), "80")); err != nil {
		http.Error(w, "airlock: "+err.Error(), http.StatusForbidden)
		return
	}
	// A host no name of this grant is bound to: the proxy forwards it and puts nothing into it,
	// exactly as the tunnel above carries what it cannot read.
	proxy := &httputil.ReverseProxy{
		Transport:     s.upstream,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = r.URL
			pr.Out.Header.Del("Proxy-Authorization")
			pr.Out.Header.Del("Proxy-Connection")
		},
		ErrorHandler: s.badGateway(host, r.URL.Path),
	}
	proxy.ServeHTTP(w, r)
}

// badGateway answers a service that could not be reached or did not answer. The error is the
// transport's own and carries no header of the request, so nothing of the grant is in it.
func (s *Server) badGateway(host, path string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, _ *http.Request, err error) {
		s.log.Warn("airlock proxy: upstream", "host", host, "path", path, "error", err)
		http.Error(w, "airlock: "+host+": "+err.Error(), http.StatusBadGateway)
	}
}

// boundName reports whether host is a name this connection's grant may carry a secret to: at
// least one of its names has an entry in the rules file whose hosts bind it. It says nothing
// about a port, so it is the test for plain HTTP, where the refusal is that the value may not
// cross in the clear at all, whatever port it was asked for on.
func (s *Server) boundName(grant requests.AirlockConnection, host string) bool {
	for name := range grant.Names {
		if entry := s.rules.Lookup(name); entry != nil && airlock.HostBound(entry.Hosts, host) {
			return true
		}
	}
	return false
}

// hijack takes the client's connection over from the outer server, with whatever the client
// wrote behind its CONNECT in front of it: a client that did not wait for the "200 Connection
// established" leaves its first bytes in net/http's own reader, where nothing below would see
// them.
func hijack(hijacker http.Hijacker) (net.Conn, error) {
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, err
	}
	if n := buffered.Reader.Buffered(); n > 0 {
		ahead, err := buffered.Reader.Peek(n)
		if err != nil {
			conn.Close()
			return nil, err
		}
		// Peek answers a window into net/http's own buffer, so the bytes are copied out of it
		// before that buffer is anybody else's.
		return &prefixConn{Conn: conn, rest: bytes.NewReader(slices.Clone(ahead))}, nil
	}
	return conn, nil
}

// slogWriter is net/http's ErrorLog as one slog line each.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	w.log.Warn("airlock proxy: " + strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// hostHeaderOf is the Host a request on target carries, and the Host the proxy sends upstream:
// the authority with the default HTTPS port left off, as every HTTP client writes it.
func hostHeaderOf(target string) string {
	return strings.TrimSuffix(target, ":443")
}
