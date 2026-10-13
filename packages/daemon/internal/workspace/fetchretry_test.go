package workspace

import (
	"context"
	"errors"
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

// Which clone failures Fetch retries: git's fatal exit with stderr naming a connection that failed
// or broke, or a status a server may not answer again. The texts are git's and curl's own: the
// first three as the production fetch printed them; the statuses and the cut transfer as git
// printed them against a local git http-backend over TLS; the rest curl's error strings, the TLS
// ones both GnuTLS's (whose libcurl Debian's git links) and OpenSSL's.
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

// testClock is the fetch's clock in a test: each wait passes at once and moves it on by its length,
// as advance does for the time an attempt takes.
type testClock struct {
	now   time.Time
	waits []time.Duration
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)}
}

func (c *testClock) Now() time.Time { return c.now }

func (c *testClock) Sleep(ctx context.Context, d time.Duration) error {
	c.advance(d)
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

// A clone that keeps failing quickly is retried after each backoff wait until a retry would start
// past FetchTimeout, which Fetch does not make: it fails, naming the last attempt and the bound.
// The waits are 5 s, 15 s and 30 s, then a minute each, so 33 attempts fit: the 33rd starts at
// 29 min 50 s, and a retry after it would start at 30 min 50 s.
func TestFetchStopsRetryingAtItsBound(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, result: fatal("error: RPC failed; HTTP 408 curl 22 The requested URL returned error: 408\nfatal: expected 'packfile'\n")}
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }

	_, err := Fetch(context.Background(), run, req)
	want := "clone attempt 33 failed, and a retry in 1m0s would start past the fetch's 30m0s bound"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "HTTP 408") {
		t.Fatalf("Fetch = %v, want the 408 and %q", err, want)
	}
	if len(run.attempts) != 33 || len(logged) != 32 {
		t.Fatalf("Fetch cloned %d times and logged %d retries, want 33 and 32", len(run.attempts), len(logged))
	}
	if elapsed := clonesUnderTheBound(t, run, clock); elapsed != 29*time.Minute+50*time.Second {
		t.Errorf("the fetch gave up %s in, want 29m50s", elapsed)
	}
}

// A clone whose every attempt takes as long as the production fetch's connect timeout (127 s) is
// given what remains of FetchTimeout each time, and the eleventh, started 29 min in with less than
// that left, is killed at the bound itself: Fetch never runs past FetchTimeout, which the daemon's
// registration deadline already allows a launching pod (sandbox.Runtime.ProvisionBound).
func TestFetchKillsTheAttemptThatReachesItsBound(t *testing.T) {
	clock := newTestClock()
	run := &failingClone{clock: clock, took: 127010 * time.Millisecond, result: fatal("fatal: unable to access 'https://github.com/acme/widgets/': Failed to connect to github.com port 443 after 127010 ms: Couldn't connect to server\n")}
	req := fetchRequest(t)
	req.Clock = clock

	_, err := Fetch(context.Background(), run, req)
	if err == nil || !strings.Contains(err.Error(), "command timed out: git clone") {
		t.Fatalf("Fetch = %v, want the last attempt's timeout", err)
	}
	if len(run.attempts) != 11 {
		t.Fatalf("Fetch cloned %d times, want 11", len(run.attempts))
	}
	if elapsed := clonesUnderTheBound(t, run, clock); elapsed != FetchTimeout {
		t.Errorf("the fetch gave up %s in, want at its bound, %s", elapsed, FetchTimeout)
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
		{"no log", func(r *FetchRequest) { r.Log = nil }, "workspace fetch request names no log"},
		{"no clock", func(r *FetchRequest) { r.Clock = nil }, "workspace fetch request names no clock"},
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

// flakyGitServer serves remote, a bare repository, over TLS through git's own http-backend, and
// fails its first pack-protocol requests (the POSTs), one way each: answered with that status, or
// for 0 cut off partway through the body, its connection closed.
func flakyGitServer(t *testing.T, remote string, failures ...int) *httptest.Server {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the flaky server runs a real git http-backend: %v", err)
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(remote), "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		failure := -1
		if r.Method == http.MethodPost && len(failures) > 0 {
			failure, failures = failures[0], failures[1:]
		}
		mu.Unlock()
		switch failure {
		case -1:
			backend.ServeHTTP(w, r)
		case 0:
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
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("cut the answer short: %v", err)
				return
			}
			_ = conn.Close()
		default:
			http.Error(w, "flaky", failure)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// retargetedClone runs Fetch's clone of github.com/acme/widgets against url instead.
type retargetedClone struct {
	Runner
	url string
}

func (r retargetedClone) Run(ctx context.Context, command Command) (Result, error) {
	if commandWith(command.Argv, "git", "clone", "--bare", "--quiet", "https://github.com/acme/widgets") {
		command.Argv = slices.Clone(command.Argv)
		command.Argv[4] = r.url
	}
	return r.Runner.Run(ctx, command)
}

// Against a real git and curl, the two kinds of failure the production fetch met — a status GitHub
// may not answer again on a request of the transfer, and a transfer the connection cut short — are
// each told transient from what git prints, and the third attempt fills the feed.
func TestFetchRetriesWhatARealGitPrintsForATransientFailure(t *testing.T) {
	remote := localBareRemote(t)
	server := flakyGitServer(t, remote, http.StatusRequestTimeout, 0)
	trustHangingServerCA(t, server)
	clock := newTestClock()
	req := fetchRequest(t)
	req.Clock = clock
	var logged []string
	req.Log = func(line string) { logged = append(logged, line) }
	run := retargetedClone{Runner: NewRunner(testTimeout, testTools(t)), url: server.URL + "/" + filepath.Base(remote)}

	feed, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(logged) != 2 || !strings.Contains(logged[0], "error: RPC failed; HTTP 408 curl 22") || !strings.Contains(logged[1], "error: RPC failed; curl 18 transfer closed with outstanding read data remaining") {
		t.Fatalf("Fetch logged %q, want the 408, then the cut transfer", logged)
	}
	if !slices.Equal(clock.waits, []time.Duration{5 * time.Second, 15 * time.Second}) {
		t.Fatalf("Fetch waited %v, want 5s then 15s", clock.waits)
	}
	if main := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+remote, "rev-parse", "main")) {
		t.Errorf("the feed's main is %s, not the remote's", main)
	}
}
