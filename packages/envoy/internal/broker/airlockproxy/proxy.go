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
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"slices"
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
	// Dial is how every upstream connection is made; nil is a net.Dialer. A test replaces it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now is the clock the proxy reads to tell a minted value that is about to stop working
	// from one that is not (mintAgainBefore); nil is time.Now. A test drives it rather than
	// waiting out a token's life.
	Now func() time.Time
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
	// net.DefaultResolver's lookup in production and a map in a test, which is how a test
	// drives a public name at a stand-in service on loopback without the loopback refusal
	// below standing in its way.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	// outer serves the bound listener: CONNECT and absolute-URL proxy requests. inner serves
	// what the outer one decrypted, through innerLn.
	outer   *http.Server
	inner   *http.Server
	innerLn *chanListener
	// upstream carries every request to a service. Its TLSClientConfig trusts the host's roots
	// as any client does; a test puts its stand-in service's own certificate in that pool.
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
// it was opened for and the grant whose password opened it.
type connInfo struct {
	// target is the CONNECT authority, always with a port; host is its host alone, ASCII
	// lower-cased, which is the airlock.Request's ConnectHost.
	target string
	host   string
	grant  requests.AirlockConnection
}

// connKey is the context key connInfo travels under.
type connKey struct{}

// New builds the proxy. Nothing is bound and nothing is served until Serve.
func New(o Options) *Server {
	s := &Server{
		rules:   o.Rules,
		grants:  o.Grants,
		ca:      o.CA,
		idle:    o.Idle,
		log:     o.Log,
		dial:    o.Dial,
		now:     o.Now,
		resolve: resolveHost,
		innerLn: newChanListener(),
		open:    map[io.Closer]struct{}{},
		values:  map[string]*grantValues{},
		access:  map[accessKey]*accessHeld{},
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		s.dial = dialer.DialContext
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.upstream = &http.Transport{
		// Through the same seam as every other dial, so one replacement in a test covers a
		// tunnel and a bound host alike.
		DialContext:         func(ctx context.Context, network, addr string) (net.Conn, error) { return s.dial(ctx, network, addr) },
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
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
		s.serveConnect(w, r, grant)
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

// serveConnect answers one CONNECT: a bound host is decrypted and read, any other host is a
// tunnel. Both are refused before any dial when the host resolves somewhere this proxy never
// goes.
func (s *Server) serveConnect(w http.ResponseWriter, r *http.Request, grant requests.AirlockConnection) {
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
	host = asciiLower(host)
	target = net.JoinHostPort(host, port)
	if err := s.refuseLocalAddress(r.Context(), host); err != nil {
		http.Error(w, "airlock: "+err.Error(), http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "airlock: this connection cannot be taken over", http.StatusInternalServerError)
		return
	}
	if s.boundHost(grant, host) {
		s.connectBound(hijacker, grant, target, host)
		return
	}
	s.connectTunnel(w, r, hijacker, target)
}

// connectBound answers a CONNECT to a host one of the grant's names is bound to: the proxy
// opens the TLS itself, with a certificate of its own authority, and hands the decrypted
// connection to the inner server.
func (s *Server) connectBound(hijacker http.Hijacker, grant requests.AirlockConnection, target, host string) {
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
	s.conns.Store(connection, connInfo{target: target, host: host, grant: grant})
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
// command's own HTTP client reports, rather than a tunnel that opens and then dies.
func (s *Server) connectTunnel(w http.ResponseWriter, r *http.Request, hijacker http.Hijacker, target string) {
	upstream, err := s.dial(r.Context(), "tcp", target)
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
	// The idle limit on the command's side alone: every byte of this tunnel is read from that
	// connection or written to it, so one wrapper sees both directions.
	bounded := newIdleConn(client, s.idle)
	ends := bothEnds{client: bounded, upstream: upstream}
	s.track(ends)
	go func() {
		_, _ = io.Copy(upstream, bounded)
		// A command that has said everything it has to say - git's upload-pack, a shell
		// pipeline - closes its half and waits for the answer, so the half-close is passed on
		// rather than turned into a close of the whole tunnel.
		if closer, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
	}()
	go func() {
		_, _ = io.Copy(bounded, upstream)
		bounded.Close()
		upstream.Close()
		s.forget(ends)
	}()
}

// servePlain answers an absolute-URL proxy request, which is plain HTTP: a command that asked
// for http:// rather than https://.
func (s *Server) servePlain(w http.ResponseWriter, r *http.Request, grant requests.AirlockConnection) {
	host := asciiLower(r.URL.Hostname())
	if s.boundHost(grant, host) {
		// Plain HTTP to a bound host is refused rather than swapped into: the value would
		// cross the network in the clear, and no rule of the airlock says a secret may. The
		// command asks again over https, where the proxy's own certificate covers it.
		http.Error(w, "airlock: refusing plain http to "+host+", a host behind the airlock: a secret of this grant may reach it only inside TLS, so ask for it over https", http.StatusForbidden)
		return
	}
	if err := s.refuseLocalAddress(r.Context(), host); err != nil {
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

// boundHost reports whether host is one this connection's grant may carry a secret to: at least
// one of its names has an entry in the rules file whose hosts bind it. A host no name binds is
// a tunnel, whatever the file says about other secrets.
func (s *Server) boundHost(grant requests.AirlockConnection, host string) bool {
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

// asciiLower lower-cases A-Z alone, the same narrow fold internal/broker/airlock makes and for
// the same reason: strings.ToLower folds U+212A KELVIN SIGN onto "k", so a CONNECT to
// "githu\u212a.com" lower-cased that way would reach - and be bound by - the entry for
// "githuk.com". Host names are ASCII, and a name that is not is no host of any rule.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
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
