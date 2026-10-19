package workspace

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Which clone failures Fetch retries: git's fatal exit with a cause naming a connection that failed
// or broke, or a status a server may not answer again. The texts are git's and curl's own: the
// first three as the production fetch printed them; the statuses and the cut transfer as git
// printed them against a local git http-backend over TLS; the rest curl's error strings, the TLS
// ones both GnuTLS's (whose libcurl Debian's git links) and OpenSSL's. A curl error is told the
// same way whichever request met it.
func TestATransientCloneFailureIsToldFromAPermanentOne(t *testing.T) {
	const access = "fatal: unable to access 'https://github.com/acme/widgets/': "
	for _, tc := range []struct {
		name      string
		result    Result
		transient bool
	}{
		{"an HTTP/2 stream cancelled mid-pack", fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n"), true},
		{"a 408 on the pack request", fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n"), true},
		{"a connect that timed out", fatal(access + "Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n"), true},
		{"a transfer cut short", fatal("error: RPC failed; curl 18 transfer closed with outstanding read data remaining\nerror: 32 bytes of body are still expected\nfatal: expected flush after ref listing\n"), true},
		{"a 502 on a request of the transfer", fatal("error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502\nfatal: expected flush after ref listing\n"), true},
		{"a TLS read that failed mid-transfer", fatal("error: RPC failed; curl 56 GnuTLS recv error (-9): Error decoding the received TLS packet.\nfatal: early EOF\n"), true},
		{"a 408 on the first request", fatal("remote: nope\n" + access + "The requested URL returned error: 408\n"), true},
		{"a 429 on the first request", fatal(access + "The requested URL returned error: 429\n"), true},
		{"a 500 on the first request", fatal(access + "The requested URL returned error: 500\n"), true},
		{"a 503 on the first request", fatal(access + "The requested URL returned error: 503\n"), true},
		{"a host that did not resolve", fatal(access + "Could not resolve host: github.com\n"), true},
		{"a connection refused", fatal(access + "Failed to connect to github.com port 443 after 3 ms: Couldn't connect to server\n"), true},
		{"a connection reset", fatal(access + "Recv failure: Connection reset by peer\n"), true},
		{"an empty reply", fatal(access + "Empty reply from server\n"), true},
		{"a request that timed out", fatal(access + "Operation timed out after 300000 milliseconds with 0 out of 0 bytes received\n"), true},
		{"a TLS handshake closed under GnuTLS", fatal(access + "gnutls_handshake() failed: The TLS connection was non-properly terminated.\n"), true},
		{"a TLS handshake closed under OpenSSL", fatal(access + "OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443\n"), true},
		{"an HTTP/2 stream on the first request", fatal(access + "HTTP/2 stream 1 was not closed cleanly: INTERNAL_ERROR (err 2)\n"), true},
		{"a pack stream that ended early", fatal("fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output\n"), true},

		{"the token refused", fatal("remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/acme/widgets/'\n"), false},
		{"no credential reached git", fatal("fatal: could not read Username for 'https://github.com': terminal prompts disabled\n"), false},
		{"the repository not found", fatal("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n"), false},
		{"a 403 on the first request", fatal("remote: Write access to repository not granted.\n" + access + "The requested URL returned error: 403\n"), false},
		{"a 403 on a request of the transfer", fatal("error: RPC failed; HTTP 403 curl 22 The requested URL returned error: 403\nfatal: expected flush after ref listing\n"), false},
		{"a certificate GnuTLS did not verify", fatal(access + "server certificate verification failed. CAfile: /etc/ssl/certs/ca-certificates.crt CRLfile: none\n"), false},
		{"a certificate OpenSSL did not verify", fatal(access + "SSL certificate problem: unable to get local issuer certificate\n"), false},
		{"a certificate refused on a request of the transfer", fatal("error: RPC failed; curl 60 SSL certificate problem: unable to get local issuer certificate\nfatal: expected flush after ref listing\n"), false},
		{"a write that failed on a request of the transfer", fatal("error: RPC failed; curl 23 Failure writing output to destination\nfatal: early EOF\n"), false},
		{"a curl error nobody has seen on a request of the transfer", fatal("error: RPC failed; curl 999 something nobody has seen\nfatal: early EOF\n"), false},
		{"a destination already there", fatal("fatal: destination path '/legion/feed/acme/widgets.git.clone-1234' already exists and is not an empty directory.\n"), false},
		{"a full disk", fatal("fatal: write error: No space left on device\nfatal: fetch-pack: invalid index-pack output\n"), false},
		{"a transient text under another exit", Result{ExitCode: 1, Stderr: "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"}, false},
		{"a clone killed by a signal", Result{ExitCode: -1, Stderr: "error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n"}, false},
		{"a clone that timed out", Result{ExitCode: -1, TimedOut: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientCloneFailure(tc.result); got != tc.transient {
				t.Fatalf("transientCloneFailure(%q, exit %d) = %t, want %t", tc.result.Stderr, tc.result.ExitCode, got, tc.transient)
			}
		})
	}
}

// fatal is a clone git ended with its fatal status, 128, printing stderr.
func fatal(stderr string) Result { return Result{ExitCode: 128, Stderr: stderr} }

// testClock is the fetch's clock in a test: each wait passes at once and moves it on by its length
// and overshoot, as a timer firing late does, and as advance does for the time an attempt takes.
type testClock struct {
	now       time.Time
	overshoot time.Duration
	waits     []time.Duration
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.advance(d + c.overshoot)
	c.waits = append(c.waits, d)
	return ctx.Err()
}

func (c *testClock) advance(d time.Duration) { c.now = c.now.Add(d) }

// failingClone is a runner whose every clone takes took of clock's time and then fails with
// result — or, given less time than that, runs to its timeout and is killed there.
type failingClone struct {
	clock  *testClock
	took   time.Duration
	result Result

	attempts []cloneAttempt
}

// cloneAttempt is one clone failingClone ran: when it started, by the clock, and its timeout.
type cloneAttempt struct {
	at      time.Time
	timeout time.Duration
}

func (r *failingClone) Timeout() time.Duration { return testTimeout }

func (r *failingClone) Run(_ context.Context, command Command) (Result, error) {
	r.attempts = append(r.attempts, cloneAttempt{at: r.clock.Now(), timeout: command.Timeout})
	if command.Timeout <= r.took {
		r.clock.advance(command.Timeout)
		return Result{ExitCode: -1, TimedOut: true}, nil
	}
	r.clock.advance(r.took)
	return r.result, nil
}

// clonesUnderTheBound fails the test unless each of run's clones was given what remained of
// FetchTimeout from the first one's start, and returns how long the fetch ran by clock.
func clonesUnderTheBound(t *testing.T, run *failingClone, clock *testClock) time.Duration {
	t.Helper()
	start := run.attempts[0].at
	for i, attempt := range run.attempts {
		if want := FetchTimeout - attempt.at.Sub(start); attempt.timeout != want {
			t.Errorf("clone attempt %d started %s in with timeout %s, want what remained of the bound, %s", i+1, attempt.at.Sub(start), attempt.timeout, want)
		}
	}
	elapsed := clock.Now().Sub(start)
	if elapsed > FetchTimeout {
		t.Errorf("the fetch ran %s, past its %s bound", elapsed, FetchTimeout)
	}
	return elapsed
}

// A clone that keeps failing quickly, as one does through a GitHub outage, is retried once after
// each backoff wait and no more: the fifth transient failure ends the fetch, 1 min 50 s in, naming
// the failure and the retry limit, rather than holding the launch for the whole FetchTimeout.
func TestFetchStopsAfterItsLastRetry(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("fatal: unable to access 'https://github.com/acme/widgets/': The requested URL returned error: 503\n")}
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 5 failed, and the fetch retries a clone at most 4 times"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "returned error: 503") {
		t.Fatalf("Fetch = %v, want the 503 and %q", err, want)
	}
	if len(run.attempts) != 5 || len(logged) != 4 {
		t.Fatalf("Fetch cloned %d times and logged %d retries, want 5 and 4", len(run.attempts), len(logged))
	}
	if !slices.Equal(clock.waits, fetchBackoff) {
		t.Fatalf("Fetch waited %v, want %v", clock.waits, fetchBackoff)
	}
	if elapsed := clonesUnderTheBound(t, run, clock); elapsed != time.Minute+50*time.Second {
		t.Errorf("the fetch gave up %s in, want 1m50s", elapsed)
	}
}

// A clone whose every attempt runs ten minutes before it breaks, as a slow transfer GitHub cuts
// does, is given what remains of FetchTimeout each time, and the third, started 20 min 20 s in
// with less than ten minutes left, is killed at the bound itself: Fetch never runs past
// FetchTimeout, which the daemon's wait for a launching pod's role launcher allows the pod while
// it provisions (sandbox.Runtime.ProvisionBound).
func TestFetchKillsTheAttemptThatReachesItsBound(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, took: 10 * time.Minute, result: fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\nfatal: early EOF\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	if err == nil || !strings.Contains(err.Error(), "command timed out: git clone") {
		t.Fatalf("Fetch = %v, want the last attempt's timeout", err)
	}
	if len(run.attempts) != 3 {
		t.Fatalf("Fetch cloned %d times, want 3", len(run.attempts))
	}
	if elapsed := clonesUnderTheBound(t, run, clock); elapsed != FetchTimeout {
		t.Errorf("the fetch gave up %s in, want at its bound, %s", elapsed, FetchTimeout)
	}
}

// A retry whose wait would end past FetchTimeout is not made: Fetch fails naming the failure and
// the bound.
func TestFetchMakesNoRetryThatWouldStartPastItsBound(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, took: FetchTimeout - 3*time.Second,
		result: fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 1 failed, and a retry in 5s would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 1 || len(clock.waits) != 0 {
		t.Fatalf("Fetch cloned %d times and waited %v, want one clone and no wait", len(run.attempts), clock.waits)
	}
}

// A wait that ends past the bound — its timer fired late — starts no further attempt, which would
// have no time at all: Fetch fails naming the last attempt's failure and the bound.
func TestFetchStartsNoAttemptOnceAWaitEndsPastItsBound(t *testing.T) {
	clock := newTestClock()
	clock.overshoot = 2 * time.Millisecond
	run := &failingClone{clock: clock, took: FetchTimeout - 5*time.Second - time.Millisecond,
		result: fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 1 failed, and its retry after the 5s wait would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 1 {
		t.Fatalf("Fetch cloned %d times, want 1", len(run.attempts))
	}
}

// A permanent failure is Fetch's answer at once: one clone, no wait, no retry logged.
func TestFetchFailsAPermanentCloneFailureAtOnce(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widgets/' not found\n")}
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	_, err := Fetch(context.Background(), run, req)
	if err == nil || !strings.Contains(err.Error(), "fatal: repository 'https://github.com/acme/widgets/' not found") {
		t.Fatalf("Fetch = %v, want git's not found", err)
	}
	if len(run.attempts) != 1 || len(clock.waits) != 0 || len(logged) != 0 {
		t.Fatalf("Fetch cloned %d times, waited %v and logged %q; want one clone, no wait, nothing logged", len(run.attempts), clock.waits, logged)
	}
}

// A fetch whose context ends during a backoff wait — the init container stopped — ends with it,
// running no further clone.
func TestFetchEndsWithItsContextDuringAWait(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: CANCEL (err 8)\n")}
	req := fetchRequest(t)
	ctx, cancel := context.WithCancel(context.Background())
	req.Clock = clock
	req.Log = func(string) { cancel() }

	_, err := Fetch(ctx, run, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch = %v, want it ended with its context", err)
	}
	if len(run.attempts) != 1 {
		t.Fatalf("Fetch cloned %d times, want 1", len(run.attempts))
	}
}

// A fetch request names the log its retries are told to and the clock they wait on; one without
// either is refused before Fetch runs anything or touches the feed.
func TestFetchRefusesARequestWithNoLogOrClock(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*FetchRequest)
		want string
	}{
		{"no log", func(r *FetchRequest) { r.Log = nil }, "workspace fetch log is required"},
		{"no clock", func(r *FetchRequest) { r.Clock = nil }, "workspace fetch clock is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			tc.edit(&req)
			if _, err := Fetch(context.Background(), run, req); err == nil || err.Error() != tc.want {
				t.Fatalf("Fetch = %v, want %q", err, tc.want)
			}
			if calls := run.Calls(); len(calls) != 0 {
				t.Errorf("Fetch ran %q before refusing", calls)
			}
			if _, err := os.Stat(req.Feed); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Fetch touched the feed before refusing: %v", err)
			}
			if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
				t.Errorf("Fetch left %v (%v) in its credential directory", entries, err)
			}
		})
	}
}

// packFailure is how the GitHub stand-in fails a pack request: with an HTTP status, or one of the
// two ways a connection breaks mid-transfer.
type packFailure int

const (
	// cutShort sends half the answer, then closes the connection: HTTP/1.1.
	cutShort packFailure = -1
	// resetStream sends half the answer, then resets the stream: HTTP/2.
	resetStream packFailure = -2
)

// gitHubStandIn is a real git http-backend that git reaches as https://github.com/acme/widgets:
// through a CONNECT proxy https_proxy names, with a certificate for github.com that
// GIT_SSL_CAINFO trusts. So Fetch's clone runs as it does in a pod, its one-shot credential
// included, which the stand-in requires over Basic auth as GitHub does, refusing any password but
// token. Each pack request (the protocol v2 `command=fetch` POST) fails the next of failures'
// ways, then the stand-in serves them.
type gitHubStandIn struct {
	remote string

	mu        sync.Mutex
	tokens    []string
	failures  []packFailure
	attempts  int
	passwords []string
}

func newGitHubStandIn(t *testing.T, http2 bool, token string, failures ...packFailure) *gitHubStandIn {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the stand-in runs a real git http-backend: %v", err)
	}
	root := t.TempDir()
	s := &gitHubStandIn{remote: localBareRemote(t), tokens: []string{token}, failures: failures}
	if err := os.Mkdir(filepath.Join(root, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(s.remote, filepath.Join(root, "acme", "widgets.git")); err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		s.mu.Lock()
		if ok {
			s.passwords = append(s.passwords, password)
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") {
				s.attempts++
			}
		}
		accepted := slices.Contains(s.tokens, password)
		s.mu.Unlock()
		if !ok || user != "x-access-token" || !accepted {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "Invalid username or token.", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the request: %v", err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if packRequest(t, r, body) {
				s.mu.Lock()
				failure := packFailure(0)
				if len(s.failures) > 0 {
					failure, s.failures = s.failures[0], s.failures[1:]
				}
				s.mu.Unlock()
				if failure != 0 {
					failPack(t, w, r, backend, failure)
					return
				}
			}
		}
		backend.ServeHTTP(w, r)
	}))
	server.EnableHTTP2 = http2
	ca, leaf := gitHubCertificate(t)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	server.StartTLS()
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", caFile)
	t.Setenv("https_proxy", "http://"+connectProxy(t, server.Listener.Addr().String()))
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")
	return s
}

// packRequest is whether r, whose body is body, is a protocol v2 pack request.
func packRequest(t *testing.T, r *http.Request, body []byte) bool {
	t.Helper()
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Errorf("read the gzipped request: %v", err)
			return false
		}
		if body, err = io.ReadAll(reader); err != nil {
			t.Errorf("read the gzipped request: %v", err)
			return false
		}
	}
	return bytes.Contains(body, []byte("command=fetch"))
}

// failPack answers r, a pack request, as failure says.
func failPack(t *testing.T, w http.ResponseWriter, r *http.Request, backend http.Handler, failure packFailure) {
	if failure > 0 {
		http.Error(w, "flaky", int(failure))
		return
	}
	answer := httptest.NewRecorder()
	backend.ServeHTTP(answer, r)
	for name, values := range answer.Header() {
		w.Header()[name] = values
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(answer.Code)
	body := answer.Body.Bytes()
	_, _ = w.Write(body[:len(body)/2])
	w.(http.Flusher).Flush()
	if failure == resetStream {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("cut the answer short: %v", err)
		return
	}
	_ = conn.Close()
}

// gitHubCertificate is a CA's certificate, DER, and a certificate for github.com it signed.
func gitHubCertificate(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	caKey, leafKey := key(), key()
	valid := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "stand-in CA"},
		NotBefore: valid.Add(-time.Hour), NotAfter: valid.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	ca, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "github.com"}, DNSNames: []string{"github.com"},
		NotBefore: valid.Add(-time.Hour), NotAfter: valid.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leaf, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return ca, tls.Certificate{Certificate: [][]byte{leaf}, PrivateKey: leafKey}
}

// connectProxy answers each CONNECT, whatever host it names, with a tunnel to target, and returns
// its address.
func connectProxy(t *testing.T, target string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				upstream, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer upstream.Close()
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
					return
				}
				go func() { _, _ = io.Copy(upstream, reader) }()
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
	return listener.Addr().String()
}

// attemptCount is how many clone attempts reached the stand-in: one authenticated ref
// advertisement each.
func (s *gitHubStandIn) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// accept has the stand-in take token as well as the one it was made with.
func (s *gitHubStandIn) accept(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, token)
}

// presented is every password git presented, in order.
func (s *gitHubStandIn) presented() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.passwords)
}

// tokensPresented fails the test unless git presented the one-shot credential's token on every
// authenticated request, and on at least one per attempt.
func (s *gitHubStandIn) tokensPresented(t *testing.T, token string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == 0 || len(s.passwords) < s.attempts || slices.ContainsFunc(s.passwords, func(password string) bool { return password != token }) {
		t.Fatalf("git presented %q over %d attempts, want the provisioning token on every request", s.passwords, s.attempts)
	}
}

// Against a real git and curl reaching the stand-in as github.com, the failures the production
// fetch met on the pack request — a 408, a transfer cut short, an HTTP/2 stream reset mid-pack —
// are each told transient from what git prints, and the attempt after them fills the feed. Every
// attempt presents the provisioning token.
func TestFetchRetriesWhatARealGitPrintsForATransientFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		http2    bool
		failures []packFailure
		logged   [][]string
	}{
		{
			name: "a 408, then a transfer cut short", failures: []packFailure{http.StatusRequestTimeout, cutShort},
			logged: [][]string{
				{"error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408", "expected 'packfile'"},
				{"error: RPC failed; curl 18 transfer closed with outstanding read data remaining"},
			},
		},
		{
			name: "an HTTP/2 stream reset mid-pack", http2: true, failures: []packFailure{resetStream},
			logged: [][]string{{"error: RPC failed; curl 92 HTTP/2 stream", "was not closed cleanly"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fetchRequest(t)
			github := newGitHubStandIn(t, tc.http2, testInstallationToken, tc.failures...)
			clock := newTestClock()
			req.Clock = clock
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			report, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			feed := report.Feed
			if len(logged) != len(tc.logged) {
				t.Fatalf("Fetch logged %q, want %d retries", logged, len(tc.logged))
			}
			for i, says := range tc.logged {
				t.Logf("retry %d logged: %s", i+1, logged[i])
				for _, text := range says {
					if !strings.Contains(logged[i], text) {
						t.Fatalf("retry %d logged %q, want it to name %q", i+1, logged[i], text)
					}
				}
			}
			if want := fetchBackoff[:len(tc.logged)]; !slices.Equal(clock.waits, want) {
				t.Fatalf("Fetch waited %v, want %v", clock.waits, want)
			}
			if attempts := github.attemptCount(); attempts != len(tc.logged)+1 {
				t.Fatalf("git made %d attempts, want %d", attempts, len(tc.logged)+1)
			}
			github.tokensPresented(t, testInstallationToken)
			if main := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+github.remote, "rev-parse", "main")) {
				t.Errorf("the feed's main is %s, not the remote's", main)
			}
		})
	}
}

// Against the same real git, a 403 on the pack request and a token GitHub refuses each fail the
// fetch on its first attempt: no wait, no retry logged.
func TestFetchFailsWhatARealGitPrintsForAPermanentFailureAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name, accepts string
		failures      []packFailure
		says          string
	}{
		{name: "a 403 on the pack request", failures: []packFailure{http.StatusForbidden, http.StatusForbidden}, says: "error: RPC failed; HTTP 403 curl 22"},
		{name: "the token refused", accepts: "ghs_another", says: "fatal: Authentication failed for 'https://github.com/acme/widgets/'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fetchRequest(t)
			accepts := testInstallationToken
			if tc.accepts != "" {
				accepts = tc.accepts
			}
			github := newGitHubStandIn(t, false, accepts, tc.failures...)
			clock := newTestClock()
			req.Clock = clock
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			_, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want git's %q", err, tc.says)
			}
			t.Logf("Fetch failed: %v", err)
			if attempts := github.attemptCount(); attempts != 1 || len(clock.waits) != 0 || len(logged) != 0 {
				t.Fatalf("git made %d attempts, Fetch waited %v and logged %q; want one attempt, no wait, nothing logged", attempts, clock.waits, logged)
			}
			github.tokensPresented(t, testInstallationToken)
		})
	}
}

// The provisioning token is read as its file stands before every transfer from GitHub, never kept
// from the fetch's start: the daemon keeps a provisioning pod's Secret fresh while its fetch runs,
// and the kubelet rewrites the file projected from it in place. Against a real git reaching the
// stand-in as github.com: a token file rewritten while the fetch waits on the repository cache is
// the one the delta presents, the stand-in taking that token alone; and one rewritten during a
// retry's backoff is the one the retry presents.
func TestFetchReadsTheProvisioningTokenAsItsFileStandsBeforeEveryTransfer(t *testing.T) {
	t.Run("rewritten during the wait on the repository cache", func(t *testing.T) {
		github := newGitHubStandIn(t, false, "ghs_fresh")
		served := &fakeCache{body: cacheTar(t, cacheOf(t, github.remote)), notReady: 2}
		pushRemoteBranch(t, github.remote, "release/v7.1", "newer.txt")
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.CacheURL, req.CacheToken = served.serve(t), testCacheToken
		req.Clock = &rewritingClock{testClock: newTestClock(), file: file, token: "ghs_fresh"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		report, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if report.Cloned || report.CacheBytes == 0 || served.healthAsked != 3 {
			t.Fatalf("Fetch reported %+v after %d health polls, want the cache streamed after the wait", report, served.healthAsked)
		}
		github.tokensPresented(t, "ghs_fresh")
	})
	t.Run("rewritten during a retry's backoff", func(t *testing.T) {
		github := newGitHubStandIn(t, false, "ghs_stale", http.StatusRequestTimeout)
		github.accept("ghs_fresh")
		req := fetchRequest(t)
		file := tokenFile(t, "ghs_stale")
		req.Token = tokenFrom(file)
		req.Clock = &rewritingClock{testClock: newTestClock(), file: file, token: "ghs_fresh"}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		if _, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		presented := github.presented()
		fresh := slices.Index(presented, "ghs_fresh")
		if github.attemptCount() != 2 || fresh <= 0 || slices.Contains(presented[:fresh], "ghs_fresh") || slices.Contains(presented[fresh:], "ghs_stale") {
			t.Fatalf("git presented %q over %d attempts, want the first attempt's token, then the rewritten one alone", presented, github.attemptCount())
		}
	})
}

// tokenFile is a provisioning token file holding token, as a pod mounts one.
func tokenFile(t *testing.T, token string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "LEGION_PROVISION_TOKEN")
	if err := os.WriteFile(file, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// tokenFrom is a FetchRequest.Token that reads file each time it is called.
func tokenFrom(file string) func() (string, error) {
	return func() (string, error) {
		contents, err := os.ReadFile(file)
		return strings.TrimSpace(string(contents)), err
	}
}

// rewritingClock is the test clock that, at its first wait, rewrites file to hold token: what the
// daemon's refresh and the kubelet do to a pod's token file while the fetch waits.
type rewritingClock struct {
	*testClock
	file, token string
	rewritten   bool
}

func (c *rewritingClock) Sleep(ctx context.Context, d time.Duration) error {
	if !c.rewritten {
		c.rewritten = true
		if err := os.WriteFile(c.file, []byte(c.token), 0o600); err != nil {
			return err
		}
	}
	return c.testClock.Sleep(ctx, d)
}

// A fetch killed mid-update leaves git's lock files in the feed, which outlives it on the issue's
// volume (or the cache's): the next Fetch, the feed's only writer, removes each and logs it with
// its age before it fetches, so the feed is never blocked for good, and the objects the issue's
// clone borrows through alternates are kept.
func TestFetchRemovesTheLocksAnInterruptedFetchLeftInTheFeed(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	feed := first.Feed
	objects := strings.Fields(runSetup(t, feed, "git", "--git-dir="+feed, "rev-list", "--objects", "--no-object-names", "--all"))
	locks := []string{"refs/heads/main.lock", "packed-refs.lock", "HEAD.lock", "shallow.lock"}
	for _, lock := range locks {
		if err := os.WriteFile(filepath.Join(feed, lock), []byte("interrupted\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	moved := pushRemoteBranch(t, run.remote, "main", "after-the-interruption.txt")
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch after an interrupted one: %v", err)
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); got != moved {
		t.Errorf("the feed's main is %s, want the remote's %s", got, moved)
	}
	for _, lock := range locks {
		if _, err := os.Stat(filepath.Join(feed, lock)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still in the feed (%v)", lock, err)
		}
		if !slices.ContainsFunc(logged, func(line string) bool {
			return strings.HasPrefix(line, "removed the stale git lock "+filepath.Join(feed, lock)+", last written ")
		}) {
			t.Errorf("the removal of %s was not logged: %q", lock, logged)
		}
	}
	for _, object := range objects {
		if err := exec.Command("git", "--git-dir="+feed, "cat-file", "-e", object).Run(); err != nil {
			t.Errorf("the feed lost %s, an object the clone may borrow", object)
		}
	}
}

// A feed that already holds the repository — the issue volume's feed on a relaunch, the repository
// cache's clone at each refresh — is fetched into, not cloned again: every branch the remote has
// moved, gained or lost reaches it, through the one-shot credential, bounded as the clone is, and
// the credential is gone once Fetch returns.
func TestFetchIntoAFeedThatHoldsTheRepositoryFetchesEveryBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	// The clock stands still, so the fetch is given the whole bound it shares with the reads before.
	req.Clock = newTestClock()
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "doomed", "main")
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	feed := first.Feed
	advanceRemote(t, run.remote)
	gained := pushRemoteBranch(t, run.remote, "release/v7.1", "gained.txt")
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-D", "doomed")
	before := len(run.Calls())

	again, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if again.Feed != feed {
		t.Fatalf("the second Fetch filled %s, want the first's %s", again.Feed, feed)
	}
	calls := run.Calls()[before:]
	for _, call := range calls {
		if commandWith(call.Argv, "git", "clone") {
			t.Fatalf("the second Fetch cloned again: %q", call.Argv)
		}
	}
	fetch := findCall(t, calls, "git", "--git-dir="+feed, "fetch")
	if want := []string{"git", "--git-dir=" + feed, "fetch", "--prune", "--quiet", "https://github.com/acme/widgets", "+refs/heads/*:refs/heads/*"}; !slices.Equal(fetch.Argv, want) {
		t.Fatalf("Fetch ran %q, want %q", fetch.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, fetch, FetchTimeout)
	for _, branch := range []string{"main", "release/v7.1"} {
		if got, want := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/"+branch)), strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", branch)); got != want {
			t.Errorf("the feed's %s is %s, want the remote's %s", branch, got, want)
		}
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/release/v7.1")); got != gained {
		t.Errorf("the feed's release/v7.1 is %s, want the pushed %s", got, gained)
	}
	if refs := runSetup(t, feed, "git", "--git-dir="+feed, "for-each-ref", "--format=%(refname)", "refs/heads/doomed"); refs != "" {
		t.Errorf("the branch the remote deleted is still in the feed: %q", refs)
	}
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// A fetch into a feed that holds the repository points its HEAD at the branch GitHub's HEAD names
// now, read under the same credential and bound as the fetch: GitHub renaming the default branch
// would otherwise leave HEAD naming the branch the fetch's prune removed, and an issue's clone of
// the feed would check nothing out. The move is logged; a fetch with nothing renamed moves nothing.
func TestFetchPointsTheFeedsHeadAtGitHubsDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "default branch") }) {
		t.Errorf("a fetch with nothing renamed logged a move: %q", logged)
	}
	runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
	before := len(run.Calls())

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch after the rename: %v", err)
	}
	if head := strings.TrimSpace(runSetup(t, first.Feed, "git", "--git-dir="+first.Feed, "symbolic-ref", "HEAD")); head != "refs/heads/trunk" {
		t.Errorf("the feed's HEAD names %s, want the renamed refs/heads/trunk", head)
	}
	read := findCall(t, run.Calls()[before:], "git", "--git-dir="+first.Feed, "ls-remote")
	if want := []string{"git", "--git-dir=" + first.Feed, "ls-remote", "--symref", "https://github.com/acme/widgets", "HEAD"}; !slices.Equal(read.Argv, want) {
		t.Errorf("Fetch read GitHub's HEAD with %q, want %q", read.Argv, want)
	}
	assertCredentialedEnvironmentBounded(t, read, read.Timeout)
	if want := "the default branch of acme/widgets is now trunk (HEAD named refs/heads/main)"; !slices.Contains(logged, want) {
		t.Errorf("the move was not logged as %q: %q", want, logged)
	}
}

// GitHub renaming its default branch between a fetch and that fetch's read of GitHub's HEAD names a
// branch the fetch did not bring. HEAD stays where it was, and the fetch says so, rather than naming
// nothing: a cache whose HEAD named nothing could publish no head, and a restart while GitHub is
// unreachable would end Serve. The next fetch brings the branch and moves HEAD to it.
func TestFetchLeavesHeadUntilAFetchBringsGitHubsDefaultBranch(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	head := func() string {
		return strings.TrimSpace(runSetup(t, first.Feed, "git", "--git-dir="+first.Feed, "symbolic-ref", "HEAD"))
	}
	run.beforeLsRemote = func() {
		run.beforeLsRemote = nil
		runSetup(t, t.TempDir(), "git", "--git-dir="+run.remote, "branch", "-m", "main", "trunk")
	}

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the Fetch GitHub renamed its branch under: %v", err)
	}
	if got := head(); got != "refs/heads/main" {
		t.Errorf("after a fetch that did not bring trunk, the feed's HEAD names %s, want refs/heads/main still", got)
	}
	if want := "GitHub's default branch of acme/widgets is now trunk, which this fetch did not bring: HEAD stays at refs/heads/main until a fetch brings it"; !slices.Contains(logged, want) {
		t.Errorf("the fetch did not say %q: %q", want, logged)
	}

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the next Fetch: %v", err)
	}
	if got := head(); got != "refs/heads/trunk" {
		t.Errorf("after the fetch that brought trunk, the feed's HEAD names %s, want refs/heads/trunk", got)
	}
}

// A feed repository Fetch did not write is not taken as its own. Before this release an issue's
// agents could write the issue volume's feed directory, so an upgraded volume may hold a repository
// there carrying a hook, configuration or link that the token-holding fetch would run or follow.
// Fetch takes an existing feed only in the shape it writes, and otherwise removes it, says so, and
// fills the feed afresh: no planted hook runs, and nothing planted survives.
func TestFetchTakesNoFeedItDidNotWrite(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, feed, marker string){
		"a hook": func(t *testing.T, feed, marker string) {
			writeExecutable(t, filepath.Join(feed, "hooks", "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
		},
		"core.hooksPath": func(t *testing.T, feed, marker string) {
			hooks := t.TempDir()
			writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			runSetup(t, feed, "git", "--git-dir="+feed, "config", "core.hooksPath", hooks)
		},
		"an http.proxy": func(t *testing.T, feed, _ string) {
			runSetup(t, feed, "git", "--git-dir="+feed, "config", "http.proxy", "http://127.0.0.1:1")
		},
		"alternates": func(t *testing.T, feed, _ string) {
			if err := os.WriteFile(filepath.Join(feed, "objects", "info", "alternates"), []byte(t.TempDir()+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"info/grafts": func(t *testing.T, feed, _ string) {
			if err := os.WriteFile(filepath.Join(feed, "info", "grafts"), []byte("0000000000000000000000000000000000000000\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked refs directory": func(t *testing.T, feed, _ string) {
			elsewhere := filepath.Join(t.TempDir(), "refs")
			if err := os.Rename(filepath.Join(feed, "refs"), elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(feed, "refs")); err != nil {
				t.Fatal(err)
			}
		},
		"a gitfile": func(t *testing.T, feed, _ string) {
			elsewhere := filepath.Join(t.TempDir(), "elsewhere.git")
			if err := os.Rename(feed, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(feed, []byte("gitdir: "+elsewhere+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"a symlinked owner directory": func(t *testing.T, feed, _ string) {
			owner := filepath.Dir(feed)
			elsewhere := filepath.Join(t.TempDir(), "acme")
			if err := os.Rename(owner, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, owner); err != nil {
				t.Fatal(err)
			}
		},
		// git reads a key written on its section header's line, `[section]key=value`, and ends an
		// unquoted value at `;`, so each of these sets what it names. `git config` never writes
		// this form; an agent writing the file by hand could.
		"a hooks path on a section header's line": func(t *testing.T, feed, marker string) {
			hooks := filepath.Join(filepath.Dir(feed), "h")
			writeExecutable(t, filepath.Join(hooks, "reference-transaction"), "#!/bin/sh\ntouch "+marker+"\n")
			appendConfig(t, feed, "[core]hooksPath="+hooks+";]\n")
		},
		"an include on a section header's line": func(t *testing.T, feed, _ string) {
			included := filepath.Join(filepath.Dir(feed), "c")
			if err := os.WriteFile(included, []byte("[http]\n\tproxy = http://127.0.0.1:1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			appendConfig(t, feed, "[include]path="+included+";]\n")
		},
		"an http.proxy on a section header's line": func(t *testing.T, feed, _ string) {
			appendConfig(t, feed, "[http]proxy=http://127.0.0.1:1;]\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			first, err := Fetch(context.Background(), run, req)
			if err != nil {
				t.Fatalf("the first Fetch: %v", err)
			}
			marker := filepath.Join(t.TempDir(), "the hook ran")
			plant(t, first.Feed, marker)
			advanceRemote(t, run.remote)
			before := len(run.Calls())

			again, err := Fetch(context.Background(), run, req)
			if err != nil {
				t.Fatalf("the Fetch after the plant: %v", err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a planted hook ran in the fetch")
			}
			if !slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "is not one this fetch wrote") }) {
				t.Errorf("Fetch did not say it refused the feed: %q", logged)
			}
			findCall(t, run.Calls()[before:], "git", "clone", "--bare")
			if info, err := os.Lstat(filepath.Dir(again.Feed)); err != nil || !info.IsDir() {
				t.Errorf("the feed's owner directory is %v (%v), want a directory", info, err)
			}
			if why, err := foreignFeed(again.Feed); err != nil || why != "" {
				t.Errorf("the refilled feed reads as foreign: %q (%v)", why, err)
			}
			if got, want := strings.TrimSpace(runSetup(t, again.Feed, "git", "--git-dir="+again.Feed, "rev-parse", "refs/heads/main")), strings.TrimSpace(runSetup(t, again.Feed, "git", "--git-dir="+run.remote, "rev-parse", "main")); got != want {
				t.Errorf("the refilled feed's main is %s, want the remote's %s", got, want)
			}
		})
	}
}

// git's update-server-info, which every `git repack` runs (the cache's due repack among them), writes
// info/refs and objects/info/packs through a temporary sibling named <file>_XXXXXX and renames it
// into place; a repack killed in between leaves that file. git never reads it, so a feed holding
// one is still one a fetch wrote: kept, never removed and cloned again whole.
func TestFetchKeepsAFeedHoldingAKilledRepacksServerInfoFiles(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	first, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the first Fetch: %v", err)
	}
	for _, leftover := range []string{"info/refs_XCPf7q", "objects/info/packs_cs3EN0"} {
		path := filepath.Join(first.Feed, leftover)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("0000000000000000000000000000000000000000\trefs/heads/main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := len(run.Calls())

	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if slices.ContainsFunc(logged, func(line string) bool { return strings.Contains(line, "is not one this fetch wrote") }) {
		t.Errorf("Fetch refused a feed holding only git's own leftovers: %q", logged)
	}
	for _, call := range run.Calls()[before:] {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned the feed again: %q", call.Argv)
		}
	}
}

// writeExecutable writes an executable script at path, making its directory.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// appendConfig appends text to the bare repository gitDir's configuration file as it stands.
func appendConfig(t *testing.T, gitDir, text string) {
	t.Helper()
	config, err := os.OpenFile(filepath.Join(gitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer config.Close()
	if _, err := config.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A clone that dies mid-transfer leaves nothing at the feed's repository: the clone runs in a
// temporary sibling renamed into place once git finished, so a feed on a volume that outlives the
// process (the issue's own, the cache's) never holds a half-cloned repository the next Fetch would
// take for a whole one and fetch into. A sibling a Fetch killed outright left (no defer ran) is
// removed by the next one, which clones whole.
func TestFetchLeavesNoHalfClonedRepositoryAtTheFeed(t *testing.T) {
	req := fetchRequest(t)
	run := newLocalRunner(t)
	killed := killedClone{Runner: run}
	if _, err := Fetch(context.Background(), &killed, req); err == nil || !strings.Contains(err.Error(), "killed mid-transfer") {
		t.Fatalf("Fetch = %v, want the killed clone's failure", err)
	}
	if killed.destination == "" {
		t.Fatal("the stand-in saw no clone")
	}
	feed := filepath.Join(req.Feed, "acme", "widgets.git")
	if killed.destination == feed {
		t.Fatalf("Fetch cloned straight into %s", feed)
	}
	for _, path := range []string{feed, killed.destination} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the killed clone left %s behind (%v)", path, err)
		}
	}

	stale := feed + ".clone-12345"
	if err := os.MkdirAll(filepath.Join(stale, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the next Fetch: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the next Fetch left the killed one's %s (%v)", stale, err)
	}
	if want := []string{"removed the clone an earlier fetch left at " + stale}; !slices.Equal(logged, want) {
		t.Errorf("the next Fetch logged %q, want %q", logged, want)
	}
	if main := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", "main")) {
		t.Errorf("the feed's main is %s, not the remote's", main)
	}
}

// killedClone runs every command through Runner but a `git clone`, which it fails as a clone the
// kernel killed mid-transfer would leave it: a partial repository at its destination, status 137.
type killedClone struct {
	Runner
	destination string
}

func (r *killedClone) Run(ctx context.Context, command Command) (Result, error) {
	if !commandWith(command.Argv, "git", "clone") {
		return r.Runner.Run(ctx, command)
	}
	r.destination = command.Argv[len(command.Argv)-1]
	if err := os.MkdirAll(filepath.Join(r.destination, "objects"), 0o700); err != nil {
		return Result{}, err
	}
	return Result{ExitCode: 137, Stderr: "killed mid-transfer"}, nil
}

// Nothing ever collects the feed's garbage: provisioning clones the issue's clone with its objects
// in the feed (objects/info/alternates), so an object the feed dropped — one only a branch GitHub
// deleted reached — would be gone from the clone too. And every fetch into the feed keeps what it
// brings as a pack, never loose objects, so the repository cache's clone streams packs alone. The
// feed's own configuration says both before the feed is in place, and a later fetch into it
// writes no loose object.
func TestFetchTurnsTheFeedsGarbageCollectionOffAndKeepsPacks(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	feed := report.Feed
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false", "fetch.unpackLimit": "1"} {
		if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "config", "--get", key)); got != want {
			t.Errorf("the feed's %s is %q, want %q", key, got, want)
		}
	}
	runSetup(t, feed, "git", "--git-dir="+feed, "repack", "-a", "-d", "--quiet")
	pushRemoteBranch(t, run.remote, "main", "one-small-change.txt")
	if _, err := Fetch(context.Background(), run, req); err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if loose := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "count-objects")); !strings.HasPrefix(loose, "0 objects") {
		t.Errorf("the fetch into the feed left %s", loose)
	}
}

// What of a bare repository the cache streams and a feed takes: its refs and its packs, nothing
// the feed rebuilds itself from them (an index, a reverse index, a bitmap, a commit-graph), no
// loose object, nothing of its own, nothing that would point it at objects elsewhere, and nothing
// git is still writing.
func TestFeedEntryIsTheRepositorysRefsAndPacksAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  bool
		want bool
	}{
		{"HEAD", false, true},
		{"packed-refs", false, true},
		{"refs", true, true},
		{"refs/heads", true, true},
		{"refs/heads/release/v7.1", false, true},
		{"objects", true, true},
		{"objects/pack", true, true},
		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.pack", false, true},

		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.idx", false, false},
		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.rev", false, false},
		{"objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d.bitmap", false, false},
		{"objects/pack/multi-pack-index", false, false},
		{"objects/pack/pack-x.pack", false, false},
		{"objects/ab", true, false},
		{"objects/ab/cdef", false, false},
		{"objects/info", true, false},
		{"objects/info/packs", false, false},
		{"objects/info/commit-graph", false, false},
		{"objects/info/alternates", false, false},
		{"objects/info/http-alternates", false, false},
		{"config", false, false},
		{"description", false, false},
		{"hooks", true, false},
		{"hooks/pre-push.sample", false, false},
		{"info", true, false},
		{"info/exclude", false, false},
		{"FETCH_HEAD", false, false},
		{"HEAD", true, false},
		{"refs", false, false},
		{"objects/pack/tmp_pack_x", false, false},
		{"objects/incoming-x", true, false},
		{"refs/heads/main.lock", false, false},
		{"packed-refs.lock", false, false},
	} {
		if got := FeedEntry(tc.name, tc.dir); got != tc.want {
			t.Errorf("FeedEntry(%q, dir %t) = %t, want %t", tc.name, tc.dir, got, tc.want)
		}
	}
}

// cacheOf is the repository cache's clone of remote as the cache keeps it: bare, holding every
// branch remote has now in one pack, as a clone over git's transport keeps it.
func cacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "acme", "widgets.git")
	runSetup(t, t.TempDir(), "git", "clone", "--bare", "--quiet", "--no-local", remote, cache)
	return cache
}

// packOf is the one pack of the bare repository dir.
func packOf(t *testing.T, dir string) string {
	t.Helper()
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("%s holds packs %v (%v), want one", dir, packs, err)
	}
	return packs[0]
}

// corruptCacheOf is cacheOf with one byte of its pack flipped, inside an object's compressed data:
// a disk or a transfer that damaged the cache's history.
func corruptCacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := cacheOf(t, remote)
	pack := packOf(t, cache)
	body, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 0xff
	if err := os.Chmod(pack, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pack, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return cache
}

// incompleteCacheOf is cacheOf whose pack lacks one blob its history reaches: a cache that lost an
// object.
func incompleteCacheOf(t *testing.T, remote string) string {
	t.Helper()
	cache := cacheOf(t, remote)
	var kept []string
	dropped := ""
	for _, line := range strings.Split(strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "rev-list", "--objects", "--all")), "\n") {
		id, _, _ := strings.Cut(line, " ")
		kind := strings.TrimSpace(runSetup(t, cache, "git", "--git-dir="+cache, "cat-file", "-t", id))
		if kind == "blob" && dropped == "" {
			dropped = id
			continue
		}
		kept = append(kept, id)
	}
	if dropped == "" {
		t.Fatal("the remote's history holds no blob to drop")
	}
	old := packOf(t, cache)
	command := exec.Command("git", "--git-dir="+cache, "pack-objects", "--quiet", filepath.Join(cache, "objects", "pack", "pack"))
	command.Stdin = strings.NewReader(strings.Join(kept, "\n") + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pack the cache without %s: %v\n%s", dropped, err, output)
	}
	for _, suffix := range []string{".pack", ".idx", ".rev"} {
		if err := os.Remove(strings.TrimSuffix(old, ".pack") + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return cache
}

// cacheTar is what the repository cache's /tar streams of the bare repository at dir: its history
// and refs (FeedEntry), each a directory or a regular file.
func cacheTar(t *testing.T, dir string) []byte {
	t.Helper()
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if !FeedEntry(name, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		if entry.IsDir() {
			header.Name += "/"
			return archive.WriteHeader(header)
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = archive.Write(body)
		return err
	})
	if err == nil {
		err = archive.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return stream.Bytes()
}

// tarOf is a tar stream of entries, each a name and, for a regular file, its body; a name ending
// in / is a directory, and one ending in @ a symbolic link to HEAD.
func tarOf(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var stream bytes.Buffer
	archive := tar.NewWriter(&stream)
	for _, entry := range entries {
		header := &tar.Header{Name: entry[0], Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(entry[1]))}
		switch {
		case strings.HasSuffix(entry[0], "/"):
			header.Typeflag, header.Mode, header.Size = tar.TypeDir, 0o755, 0
		case strings.HasSuffix(entry[0], "@"):
			header.Name, header.Typeflag, header.Linkname, header.Size = strings.TrimSuffix(entry[0], "@"), tar.TypeSymlink, "HEAD", 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := archive.Write([]byte(entry[1])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return stream.Bytes()
}

// testCacheToken is the bearer the fake cache takes and fetchRequest's cache requests present.
const testCacheToken = "cache-token-of-acme-widgets"

// fakeCache stands in for the repository cache's server: GET /tar must carry the bearer
// testCacheToken, or it is answered 401; GET /healthz answers any request, 503 for its first
// notReady polls and then 200 ready, recording the Authorization each carried. An answered request
// names the cache's repository, acme/widgets unless repo or tarRepo says otherwise, in
// CacheRepositoryHeader. It counts each.
type fakeCache struct {
	body     []byte
	notReady int
	repo     string
	tarRepo  string

	mu                sync.Mutex
	healthAsked       int
	healthCredentials []string
	tarAsked          int
}

func (c *fakeCache) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	repo := cmp.Or(c.repo, "acme/widgets")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		c.healthAsked++
		if credential := r.Header.Get("Authorization"); credential != "" {
			c.healthCredentials = append(c.healthCredentials, credential)
		}
		w.Header().Set(CacheRepositoryHeader, repo)
		if c.healthAsked <= c.notReady {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ready":false,"repo":"` + repo + `","bytes":0}`))
			return
		}
		_, _ = w.Write([]byte(`{"ready":true,"repo":"` + repo + `","bytes":1}`))
	case r.Method == http.MethodGet && r.URL.Path == "/tar":
		c.tarAsked++
		if r.Header.Get("Authorization") != "Bearer "+testCacheToken {
			http.Error(w, "the repository cache's token is required", http.StatusUnauthorized)
			return
		}
		w.Header().Set(CacheRepositoryHeader, cmp.Or(c.tarRepo, repo))
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(c.body)
	default:
		http.NotFound(w, r)
	}
}

// serve starts c and returns its address, closed when the test ends.
func (c *fakeCache) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(c)
	t.Cleanup(server.Close)
	return server.URL
}

// With a repository cache, a feed that does not hold the repository yet is unpacked from the
// cache's tar and fetched from GitHub for what the cache lacks: the feed has the cache's refs and
// the remote's newer one, the report counts the tar's bytes and the few GitHub brought, and nothing
// was cloned from GitHub. The feed's configuration is its own, gc off, whatever the cache's was.
func TestFetchStreamsTheRepositoryCacheThenFetchesTheDeltaFromGitHub(t *testing.T) {
	run := newLocalRunner(t)
	cache := cacheOf(t, run.remote)
	body := cacheTar(t, cache)
	served := &fakeCache{body: body}
	moved := pushRemoteBranch(t, run.remote, "release/v7.1", "newer.txt")
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken = served.serve(t), testCacheToken

	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	feed := filepath.Join(req.Feed, "acme", "widgets.git")
	if report.Feed != feed || report.Cloned {
		t.Fatalf("Fetch reported %+v, want the feed %s filled from the cache", report, feed)
	}
	if report.CacheBytes != int64(len(body)) || report.CacheDuration <= 0 || report.VerifyDuration <= 0 {
		t.Errorf("the report counts %d bytes in %s from the cache, verified in %s, want the tar's %d and both times", report.CacheBytes, report.CacheDuration, report.VerifyDuration, len(body))
	}
	if report.GitHubBytes <= 0 || report.GitHubBytes >= 64<<10 || report.Refs != 1 {
		t.Errorf("the report counts %d bytes and %d refs from GitHub, want the one new branch's few", report.GitHubBytes, report.Refs)
	}
	for _, branch := range []string{"main", "release/v7.1"} {
		if got, want := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/"+branch)), strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", branch)); got != want {
			t.Errorf("the feed's %s is %s, want the remote's %s", branch, got, want)
		}
	}
	if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/release/v7.1")); got != moved {
		t.Errorf("the feed's release/v7.1 is %s, want the remote's newer %s", got, moved)
	}
	runSetup(t, feed, "git", "--git-dir="+feed, "fsck", "--connectivity-only")
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned from GitHub with a cache to stream: %q", call.Argv)
		}
	}
	var fetch Command
	for _, call := range run.Calls() {
		if len(call.Argv) > 2 && call.Argv[0] == "git" && strings.HasPrefix(call.Argv[1], "--git-dir="+feed+cloneSuffix) && call.Argv[2] == "fetch" {
			fetch = call
		}
	}
	if fetch.Argv == nil {
		t.Fatalf("Fetch fetched no delta from GitHub into the unpacked cache: %q", run.Calls())
	}
	assertCredentialedEnvironmentBounded(t, fetch, fetch.Timeout)
	if left := FetchTimeout - report.CacheDuration - report.VerifyDuration; fetch.Timeout > left || fetch.Timeout < left-time.Minute {
		t.Errorf("the delta was given %s, want what remained of the one %s bound after the stream and its verification: at most %s", fetch.Timeout, FetchTimeout, left)
	}
	for key, want := range map[string]string{"gc.auto": "0", "maintenance.auto": "false"} {
		if got := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "config", "--get", key)); got != want {
			t.Errorf("the feed's %s is %q, want %q", key, got, want)
		}
	}
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// A fresh feed comes from the repository cache alone: while the cache is not ready, Fetch waits on
// it, asking its /healthz every CachePoll and logging what it answered, then streams it once it is
// ready. Nothing is cloned from GitHub meanwhile. The health polls carry no credential: the cache's
// token goes with the tar alone.
func TestFetchWaitsForTheRepositoryCacheToBeReady(t *testing.T) {
	run := newLocalRunner(t)
	served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote)), notReady: 3}
	req := fetchRequest(t)
	req.CacheURL, req.CacheToken = served.serve(t), testCacheToken
	clock := newTestClock()
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if report.Cloned || report.CacheBytes == 0 || served.tarAsked != 1 || served.healthAsked != 4 {
		t.Fatalf("Fetch reported %+v after %d health polls and %d tars, want the cache streamed once after 4 polls", report, served.healthAsked, served.tarAsked)
	}
	if want := []time.Duration{CachePoll, CachePoll, CachePoll}; !slices.Equal(clock.waits, want) {
		t.Errorf("Fetch waited %v between polls, want %v", clock.waits, want)
	}
	if len(served.healthCredentials) != 0 {
		t.Errorf("Fetch's health polls carried %q, want no credential", served.healthCredentials)
	}
	want := "waiting for the repository cache " + req.CacheURL + " to be ready: GET " + req.CacheURL + "/healthz answered 503 Service Unavailable"
	if len(logged) == 0 || !strings.HasPrefix(logged[0], want) {
		t.Errorf("Fetch logged %q, want the wait first: %q", logged, want)
	}
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned from GitHub while it waited on the cache: %q", call.Argv)
		}
	}
}

// A cache that never becomes ready within CacheWaitTimeout fails the fetch, naming the cache and
// what it last answered: there is no full clone from GitHub instead, so a cache that is down never
// costs each new issue a full download.
func TestFetchFailsWhenTheRepositoryCacheIsNeverReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  func(t *testing.T) string
		says string
	}{
		{"a cache answering not ready", func(t *testing.T) string { return (&fakeCache{notReady: 1 << 30}).serve(t) }, "503 Service Unavailable"},
		{"a cache that does not answer", func(t *testing.T) string {
			server := httptest.NewServer(http.NotFoundHandler())
			server.Close()
			return server.URL
		}, "connection refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = tc.url(t), testCacheToken
			req.Clock = newTestClock()
			req.Log = func(string) {}
			_, err := Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), "the repository cache "+req.CacheURL+" was not ready within "+CacheWaitTimeout.String()) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want the wait's failure naming %q", err, tc.says)
			}
			assertNothingFilledTheFeed(t, run, req)
		})
	}
}

// The cache's stream, its verification and the delta from GitHub share one FetchTimeout, counted
// from when the cache is ready, so the fetch's waits, transfers and checks end within FetchBudget
// however long it waited. A cache ready 35 minutes in, whose pack takes 20 minutes to index and
// whose history takes 5 to check,
// leaves the delta the last 5 minutes, and the fresh feed is filled. A cache ready on the wait's
// last ask, 39 min 50 s in, whose pack takes the whole bound to index, leaves nothing to check its
// history in: the fetch fails then, 69 min 50 s in, naming the bound, and fills nothing.
func TestFetchGivesTheCachesStreamVerificationAndDeltaOneBound(t *testing.T) {
	polls := func(d time.Duration) int { return int(d / CachePoll) }
	for _, tc := range []struct {
		name     string
		notReady int
		took     map[string]time.Duration
		timeouts map[string][]time.Duration
		fails    string
		elapsed  time.Duration
	}{
		{
			name: "a cache ready 35 minutes in", notReady: polls(35 * time.Minute),
			took:     map[string]time.Duration{"index-pack": 20 * time.Minute, "fsck": 5 * time.Minute},
			timeouts: map[string][]time.Duration{"index-pack": {FetchTimeout}, "fsck": {FetchTimeout - 20*time.Minute}, "fetch": {FetchTimeout - 25*time.Minute}},
			elapsed:  35*time.Minute + 25*time.Minute,
		},
		{
			name: "a pack that takes the whole bound to index", notReady: polls(CacheWaitTimeout) - 1,
			took:     map[string]time.Duration{"index-pack": FetchTimeout},
			timeouts: map[string][]time.Duration{"index-pack": {FetchTimeout}},
			fails:    "the fetch's " + FetchTimeout.String() + " bound ran out before checking the repository cache's history is whole",
			elapsed:  CacheWaitTimeout - CachePoll + FetchTimeout,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			clock := newTestClock()
			start := clock.Now()
			slow := &slowSteps{recordingRunner: run, clock: clock, step: cacheFetchStep, took: tc.took, timeouts: map[string][]time.Duration{}}
			served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote)), notReady: tc.notReady}
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken, req.Clock, req.Log = served.serve(t), testCacheToken, clock, func(string) {}

			report, err := Fetch(context.Background(), slow, req)
			if tc.fails == "" && err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if tc.fails != "" && (err == nil || !strings.Contains(err.Error(), tc.fails)) {
				t.Fatalf("Fetch = %+v, %v; want it failed: %q", report, err, tc.fails)
			}
			if !maps.EqualFunc(slow.timeouts, tc.timeouts, slices.Equal[[]time.Duration]) {
				t.Errorf("the fetch's commands were given %v, want %v: what remained of one bound from the cache's readiness", slow.timeouts, tc.timeouts)
			}
			if elapsed := clock.Now().Sub(start); elapsed != tc.elapsed || elapsed > FetchBudget {
				t.Errorf("the fetch ran %s, want %s, within FetchBudget %s", elapsed, tc.elapsed, FetchBudget)
			}
			if tc.fails != "" {
				assertNothingFilledTheFeed(t, run, req)
			}
		})
	}
}

// cacheFetchStep names the steps of a fetch from the cache whose duration follows the
// repository's size: each pack's indexing, the history's check, and the delta from GitHub.
func cacheFetchStep(argv []string) string {
	switch {
	case slices.Contains(argv, "index-pack"):
		return "index-pack"
	case slices.Contains(argv, "fsck"):
		return "fsck"
	case len(argv) > 2 && argv[0] == "git" && argv[2] == "fetch":
		return "fetch"
	}
	return ""
}

// assertNothingFilledTheFeed fails when Fetch cloned from GitHub or left anything at, or beside,
// the feed's repository.
func assertNothingFilledTheFeed(t *testing.T, run *recordingRunner, req FetchRequest) {
	t.Helper()
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "git", "clone") {
			t.Errorf("Fetch cloned from GitHub: %q", call.Argv)
		}
	}
	if entries, err := os.ReadDir(filepath.Join(req.Feed, "acme")); err == nil && len(entries) != 0 {
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("Fetch left %v in the feed", names)
	}
}

// A cache whose tar holds an entry a feed does not take — one whose name leaves the feed, one that
// is not a regular file or directory, the cache's own configuration, an alternates file, a loose
// object, a pack's index — or whose history does not verify — a pack with a damaged object, a
// history missing an object it reaches — fails the fetch, naming the entry or the check: nothing of
// it reaches the feed, and nothing is cloned from GitHub.
func TestFetchRefusesARepositoryCacheItCannotUse(t *testing.T) {
	const pack = "objects/pack/pack-1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	for _, tc := range []struct {
		name string
		body func(t *testing.T, remote string) []byte
		says string
	}{
		{"an entry leaving the feed", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"HEAD", "ref: refs/heads/main\n"}, [2]string{"../x", "escaped\n"})
		}, `the repository cache's tar holds "../x", which is not a path inside the feed`},
		{"a symbolic link", func(t *testing.T, _ string) []byte { return tarOf(t, [2]string{"refs/heads/main@"}) },
			`the repository cache's tar holds "refs/heads/main", which is neither a regular file nor a directory`},
		{"the cache's configuration", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"config", "[core]\n\tfsmonitor = true\n"})
		},
			`the repository cache's tar holds "config", which is not one of a repository's refs or packs`},
		{"an alternates file", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"objects/info/alternates", "/elsewhere\n"})
		},
			`the repository cache's tar holds "objects/info/alternates", which is not one of a repository's refs or packs`},
		{"a loose object", func(t *testing.T, _ string) []byte {
			return tarOf(t, [2]string{"objects/ab/cdef0123456789abcdef0123456789abcdef01", "x"})
		}, `the repository cache's tar holds "objects/ab/cdef0123456789abcdef0123456789abcdef01", which is not one of a repository's refs or packs`},
		{"a pack's index", func(t *testing.T, _ string) []byte { return tarOf(t, [2]string{pack + ".idx", "x"}) },
			`the repository cache's tar holds "` + pack + `.idx", which is not one of a repository's refs or packs`},
		{"a pack with a damaged object", func(t *testing.T, remote string) []byte { return cacheTar(t, corruptCacheOf(t, remote)) },
			"which does not verify"},
		{"a history missing an object", func(t *testing.T, remote string) []byte { return cacheTar(t, incompleteCacheOf(t, remote)) },
			"the repository cache's history is not whole"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			pushRemoteBranch(t, run.remote, "main", "history.txt")
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = (&fakeCache{body: tc.body(t, run.remote)}).serve(t), testCacheToken
			_, err := Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), "fill the feed from the repository cache "+req.CacheURL) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want the cache refused naming %q", err, tc.says)
			}
			assertNothingFilledTheFeed(t, run, req)
			if _, err := os.Stat(filepath.Join(req.Feed, "acme", "x")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the tar wrote outside the feed (%v)", err)
			}
		})
	}
}

// A fetch asks only its own repository's cache, which it proves twice: it takes nothing from a
// cache whose answer names another repository — its /healthz, or the /tar itself, read before any
// of the tar's content — and it presents that cache's token for the tar, which another
// repository's cache refuses with 401. Each is refused at once, without waiting, and nothing
// reaches the feed. A cache named without its token is refused before anything is asked.
func TestFetchTakesNothingFromAnotherRepositorysCache(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cache  *fakeCache
		token  string
		says   string
		health int
		tars   int
	}{
		{"a cache that refuses the token", &fakeCache{}, "another-cache's-token", "/tar answered 401 Unauthorized", 1, 1},
		{"a cache whose health names another repository", &fakeCache{repo: "acme/gadgets"}, testCacheToken,
			"is the repository cache of acme/gadgets, not of acme/widgets", 1, 0},
		{"a cache whose tar names another repository", &fakeCache{tarRepo: "acme/gadgets"}, testCacheToken,
			"is the repository cache of acme/gadgets, not of acme/widgets", 1, 1},
		{"a cache that names no repository", &fakeCache{tarRepo: " "}, testCacheToken,
			`is the repository cache of "", not of acme/widgets`, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			tc.cache.body = cacheTar(t, cacheOf(t, run.remote))
			req := fetchRequest(t)
			req.CacheURL, req.CacheToken = tc.cache.serve(t), tc.token
			clock := newTestClock()
			req.Clock = clock
			_, err := Fetch(context.Background(), run, req)
			if err == nil || !strings.Contains(err.Error(), req.CacheURL) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Fetch = %v, want a refusal naming %q", err, tc.says)
			}
			if tc.cache.healthAsked != tc.health || tc.cache.tarAsked != tc.tars || len(clock.waits) != 0 {
				t.Errorf("Fetch asked /healthz %d and /tar %d times and waited %v, want %d, %d and no wait", tc.cache.healthAsked, tc.cache.tarAsked, clock.waits, tc.health, tc.tars)
			}
			assertNothingFilledTheFeed(t, run, req)
		})
	}

	req := fetchRequest(t)
	req.CacheURL = "http://127.0.0.1:1"
	if _, err := Fetch(context.Background(), newLocalRunner(t), req); err == nil || err.Error() != "workspace fetch: a repository cache needs its token" {
		t.Fatalf("Fetch with a cache and no token = %v", err)
	}
}

// Without a cache, Fetch clones from GitHub and the report says how many bytes that brought: what
// the repository cache's own process does. A feed that holds the repository already is fetched
// into and asks no cache, whatever cache it is given.
func TestFetchReportsWhereTheFeedCameFrom(t *testing.T) {
	run := newLocalRunner(t)
	req := fetchRequest(t)
	report, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !report.Cloned || report.CacheBytes != 0 || report.GitHubBytes <= 0 {
		t.Fatalf("Fetch without a cache reported %+v, want a clone from GitHub with its bytes", report)
	}

	served := &fakeCache{body: cacheTar(t, cacheOf(t, run.remote))}
	req.CacheURL, req.CacheToken = served.serve(t), testCacheToken
	advanceRemote(t, run.remote)
	again, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("the second Fetch: %v", err)
	}
	if again.Cloned || again.CacheBytes != 0 || again.Refs != 1 || again.GitHubBytes <= 0 || served.healthAsked+served.tarAsked != 0 {
		t.Fatalf("Fetch into a feed holding the repository reported %+v and asked the cache %d times, want a delta of one ref from GitHub and no cache", again, served.healthAsked+served.tarAsked)
	}
}
