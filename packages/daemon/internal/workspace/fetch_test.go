package workspace

import (
	"bufio"
	"bytes"
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
		{"a destination already there", fatal("fatal: destination path '/var/run/legion/feed/acme/widgets.git' already exists and is not an empty directory.\n"), false},
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
	s := &gitHubStandIn{remote: localBareRemote(t), failures: failures}
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
		s.mu.Unlock()
		if !ok || user != "x-access-token" || password != token {
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
			github := newGitHubStandIn(t, tc.http2, req.Token, tc.failures...)
			clock := newTestClock()
			req.Clock = clock
			var logged []string
			req.Log = func(line string) { logged = append(logged, line) }
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()

			feed, err := Fetch(ctx, NewRunner(testTimeout, testTools(t)), req)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
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
			github.tokensPresented(t, req.Token)
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
			accepts := req.Token
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
			github.tokensPresented(t, req.Token)
		})
	}
}
